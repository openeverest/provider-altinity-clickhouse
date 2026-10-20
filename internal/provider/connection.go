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
	"fmt"
	"net"
	"net/url"
	"strconv"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// buildConnectionDetails reports how to reach a ready Instance.
//
// host is the LoadBalancer address once one is assigned, so it works from
// outside the cluster; internal_host is always the in-cluster Service. port
// and uri are for the HTTP interface (HTTPS with TLS); the *_port keys list
// every protocol clients may use in the Instance's TLS mode, and *_node_port
// keys their node ports when exposed through a NodePort.
func buildConnectionDetails(c *controller.Context, chi *chiv1.ClickHouseInstallation) controller.ConnectionDetails {
	internalHost := chi.Status.GetEndpoint()
	if internalHost == "" {
		internalHost = fmt.Sprintf("%s.%s.svc", clientServiceName(c.Name()), c.Namespace())
	}
	additional := map[string]string{common.ConnectionKeyInternalHost: internalHost}

	ports := clientPorts(tlsMode(c))
	for key, port := range ports {
		additional[key] = strconv.Itoa(port)
	}

	host := internalHost
	svc := &corev1.Service{}
	if found, err := getOptional(c, svc, clientServiceName(c.Name())); err == nil && found {
		if address := loadBalancerAddress(svc); address != "" {
			host = address
		}
		if svc.Spec.Type == corev1.ServiceTypeNodePort {
			for key, port := range ports {
				if nodePort := nodePortFor(svc, port); nodePort != 0 {
					additional[key+common.ConnectionKeyNodePortSuffix] = strconv.Itoa(int(nodePort))
				}
			}
		}
	}

	scheme, port := "http", common.HTTPPort
	if tlsEnabled(c) {
		scheme, port = "https", common.HTTPSPort
		// Clients need the CA to verify a provider-managed certificate.
		if ca := readCACert(c); ca != "" {
			additional[common.ConnectionKeyCACert] = ca
		}
	}

	username, password := readCredentials(c)
	uri := url.URL{
		Scheme: scheme,
		User:   url.UserPassword(username, password),
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   "/",
	}
	return controller.ConnectionDetails{
		Type:                 "clickhouse",
		Provider:             common.ProviderName,
		Host:                 host,
		Port:                 strconv.Itoa(port),
		Username:             username,
		Password:             password,
		URI:                  uri.String(),
		AdditionalProperties: additional,
	}
}

// clientPorts returns the ports clients may use in the TLS mode, by
// connection details key: required TLS leaves out the plaintext ones.
func clientPorts(tlsMode string) map[string]int {
	ports := map[string]int{}
	if tlsMode != common.TLSModeRequired {
		ports[common.ConnectionKeyHTTPPort] = common.HTTPPort
		ports[common.ConnectionKeyNativePort] = common.TCPPort
	}
	if tlsMode != "" {
		ports[common.ConnectionKeyHTTPSPort] = common.HTTPSPort
		ports[common.ConnectionKeyNativeSecurePort] = common.TCPSecurePort
	}
	return ports
}

// loadBalancerAddress returns the first hostname or, failing that, IP a
// LoadBalancer Service was assigned.
func loadBalancerAddress(svc *corev1.Service) string {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return ""
	}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.Hostname != "" {
			return ingress.Hostname
		}
	}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP
		}
	}
	return ""
}

func nodePortFor(svc *corev1.Service, port int) int32 {
	for _, p := range svc.Spec.Ports {
		if int(p.Port) == port {
			return p.NodePort
		}
	}
	return 0
}
