// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provider

import (
	"net/url"
	"testing"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

const internalEndpoint = "clickhouse-db.ns.svc.cluster.local"

func readyCHI() *chiv1.ClickHouseInstallation {
	chi := completedCHI()
	chi.Status.Endpoint = internalEndpoint
	return chi
}

func clientService(svcType corev1.ServiceType, ingress ...corev1.LoadBalancerIngress) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "clickhouse-db", Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Type: svcType},
	}
	svc.Status.LoadBalancer.Ingress = ingress
	return svc
}

func connectionDetails(t *testing.T, params string, objs ...client.Object) controller.ConnectionDetails {
	t.Helper()
	c := newTestContext(t, newTestInstance("db", "ns", params), objs...)
	_, err := ensureCredentials(c)
	require.NoError(t, err)
	return buildConnectionDetails(c, readyCHI())
}

func TestConnectionDetailsPortsFollowTLSMode(t *testing.T) {
	tests := []struct {
		name   string
		params string
		port   string
		ports  map[string]string
	}{
		{name: "no TLS", port: "8123", ports: map[string]string{
			"http_port": "8123", "native_port": "9000",
		}},
		{name: "required TLS", params: tlsRequiredParams, port: "8443", ports: map[string]string{
			"https_port": "8443", "native_secure_port": "9440",
		}},
		{name: "optional TLS", params: tlsOptionalParams, port: "8443", ports: map[string]string{
			"http_port": "8123", "native_port": "9000", "https_port": "8443", "native_secure_port": "9440",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			details := connectionDetails(t, tt.params)
			assert.Equal(t, internalEndpoint, details.Host)
			assert.Equal(t, tt.port, details.Port)
			for _, key := range []string{"http_port", "native_port", "https_port", "native_secure_port"} {
				assert.Equal(t, tt.ports[key], details.AdditionalProperties[key], key)
			}
			assert.Equal(t, internalEndpoint, details.AdditionalProperties[common.ConnectionKeyInternalHost])
		})
	}
}

func TestConnectionDetailsUseLoadBalancerAddress(t *testing.T) {
	tests := []struct {
		name    string
		ingress []corev1.LoadBalancerIngress
		want    string
	}{
		{name: "pending", want: internalEndpoint},
		{name: "IP", ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}, want: "203.0.113.7"},
		{name: "hostname preferred", ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}, {Hostname: "db.example.com"}}, want: "db.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			details := connectionDetails(t, "", clientService(corev1.ServiceTypeLoadBalancer, tt.ingress...))
			assert.Equal(t, tt.want, details.Host)
			assert.Equal(t, internalEndpoint, details.AdditionalProperties[common.ConnectionKeyInternalHost], "in-cluster clients keep the Service")
			uri, err := url.Parse(details.URI)
			require.NoError(t, err)
			assert.Equal(t, tt.want, uri.Hostname())
		})
	}
}

func TestConnectionDetailsReportNodePorts(t *testing.T) {
	svc := clientService(corev1.ServiceTypeNodePort)
	svc.Spec.Ports = []corev1.ServicePort{{Port: 8443, NodePort: 30443}, {Port: 9440, NodePort: 30440}}
	details := connectionDetails(t, tlsRequiredParams, svc)
	assert.Equal(t, internalEndpoint, details.Host)
	assert.Equal(t, "30443", details.AdditionalProperties["https_port_node_port"])
	assert.Equal(t, "30440", details.AdditionalProperties["native_secure_port_node_port"])
	assert.NotContains(t, details.AdditionalProperties, "http_port_node_port")

	// Not exposed through a NodePort: no node ports.
	plain := connectionDetails(t, tlsRequiredParams, clientService(corev1.ServiceTypeClusterIP))
	assert.NotContains(t, plain.AdditionalProperties, "https_port_node_port")
}

// User-chosen passwords may hold URI delimiters; an IPv6 address needs brackets.
func TestConnectionURIEscapesCredentialsAndIPv6(t *testing.T) {
	creds := secretWith(credentialsSecretName("db"), map[string]string{
		"username": "dba", "password": "p@ss/w:rd?#0123",
	})
	c := newTestContext(t, newTestInstance("db", "ns", ""), creds,
		clientService(corev1.ServiceTypeLoadBalancer, corev1.LoadBalancerIngress{IP: "2001:db8::7"}))
	details := buildConnectionDetails(c, readyCHI())

	uri, err := url.Parse(details.URI)
	require.NoError(t, err)
	assert.Equal(t, "dba", uri.User.Username())
	password, _ := uri.User.Password()
	assert.Equal(t, "p@ss/w:rd?#0123", password)
	assert.Equal(t, "2001:db8::7", uri.Hostname())
	assert.Equal(t, "8123", uri.Port())
}
