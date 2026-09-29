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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/components"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

func newTestContext(t *testing.T, instance *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
	t.Helper()
	return newInterceptedTestContext(t, interceptor.Funcs{}, instance, objs...)
}

func newInterceptedTestContext(t *testing.T, funcs interceptor.Funcs, instance *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, chiv1.AddToScheme(scheme))
	require.NoError(t, cmapi.AddToScheme(scheme))
	require.NoError(t, monitoringv1.AddToScheme(scheme))

	all := append([]client.Object{instance}, objs...)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).WithInterceptorFuncs(funcs).Build()
	return controller.NewContext(context.Background(), fakeClient, instance, common.ProviderName)
}

// captureCHIApply records applied CHIs instead of forwarding them: the fake
// client cannot server-side apply Altinity types (unexported fields).
func captureCHIApply(t *testing.T, applied *[]*chiv1.ClickHouseInstallation) interceptor.Funcs {
	t.Helper()
	return interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			data, err := json.Marshal(obj)
			require.NoError(t, err)
			chi := &chiv1.ClickHouseInstallation{}
			require.NoError(t, json.Unmarshal(data, chi))
			if chi.Kind != "ClickHouseInstallation" {
				return c.Apply(ctx, obj, opts...)
			}
			*applied = append(*applied, chi)
			return nil
		},
	}
}

func newTestInstance(name, namespace, engineParams string) *corev1alpha1.Instance {
	engine := corev1alpha1.ComponentSpec{Image: "clickhouse/clickhouse-server:25.3"}
	if engineParams != "" {
		engine.Parameters = &runtime.RawExtension{Raw: []byte(engineParams)}
	}
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1alpha1.InstanceSpec{
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: engine,
			},
		},
	}
}

func TestEnsureCredentialsIdempotent(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""))

	first, err := ensureCredentials(c)
	require.NoError(t, err)

	password := first.Data[common.CredentialsKeyPassword]
	require.NotEmpty(t, password)
	assert.Equal(t, common.AppUserName, string(first.Data[common.CredentialsKeyUsername]))

	digest := sha256.Sum256(password)
	assert.Equal(t, hex.EncodeToString(digest[:]), string(first.Data[common.CredentialsKeyPasswordSHA256]))

	// A second reconcile must not regenerate the password.
	second, err := ensureCredentials(c)
	require.NoError(t, err)
	assert.Equal(t, password, second.Data[common.CredentialsKeyPassword])
}

func TestSyncReappliesExistingCHI(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	engine := instance.Spec.Components[common.ComponentEngine]
	engine.Image = "clickhouse/clickhouse-server:25.8"
	instance.Spec.Components[common.ComponentEngine] = engine

	existing := &chiv1.ClickHouseInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	existing.EnsureStatus().Status = chiv1.StatusCompleted

	var applied []*chiv1.ClickHouseInstallation
	c := newInterceptedTestContext(t, captureCHIApply(t, &applied), instance, existing)

	require.NoError(t, New().Sync(c))

	require.Len(t, applied, 1)
	containers := applied[0].Spec.Templates.PodTemplates[0].Spec.Containers
	assert.Equal(t, "clickhouse/clickhouse-server:25.8", containers[0].Image)
}

func TestBuildCHIProvisionsAdminUser(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""))

	chi, err := buildCHI(c, 1)
	require.NoError(t, err)

	users := chi.Spec.Configuration.Users
	require.NotNil(t, users)
	assert.True(t, users.Has("admin/networks/ip"))
	assert.Equal(t, "1", users.Get("admin/access_management").ScalarString())

	ref := users.Get("admin/password_sha256_hex").GetSecretKeyRef()
	require.NotNil(t, ref)
	assert.Equal(t, "db-credentials", ref.Name)
	assert.Equal(t, common.CredentialsKeyPasswordSHA256, ref.Key)

	// Without TLS there are no cert files nor secure ports.
	assert.Nil(t, chi.Spec.Configuration.Files)
	assert.False(t, chi.Spec.Configuration.Settings.Has("https_port"))
}

func TestBuildCHIWithTLS(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", `{"tls":{"enabled":"enabled"}}`))

	chi, err := buildCHI(c, 2)
	require.NoError(t, err)

	files := chi.Spec.Configuration.Files
	require.NotNil(t, files)
	assert.True(t, files.Has("openssl.xml"))

	certRef := files.Get("server.crt").GetSecretKeyRef()
	require.NotNil(t, certRef)
	assert.Equal(t, "db-server-tls", certRef.Name)
	assert.Equal(t, "tls.crt", certRef.Key)

	settings := chi.Spec.Configuration.Settings
	assert.Equal(t, "8443", settings.Get("https_port").ScalarString())
	assert.Equal(t, "9440", settings.Get("tcp_port_secure").ScalarString())
}

func TestBuildConnectionDetails(t *testing.T) {
	tests := []struct {
		name         string
		engineParams string
		wantScheme   string
		wantPort     string
	}{
		{name: "plaintext", engineParams: "", wantScheme: "http", wantPort: "8123"},
		{name: "tls", engineParams: `{"tls":{"enabled":"enabled"}}`, wantScheme: "https", wantPort: "8443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestContext(t, newTestInstance("db", "ns", tt.engineParams))
			secret, err := ensureCredentials(c)
			require.NoError(t, err)
			password := string(secret.Data[common.CredentialsKeyPassword])

			chi := &chiv1.ClickHouseInstallation{}
			chi.EnsureStatus()

			details := buildConnectionDetails(c, chi)
			assert.Equal(t, common.AppUserName, details.Username)
			assert.Equal(t, password, details.Password)
			assert.Equal(t, tt.wantPort, details.Port)
			assert.Contains(t, details.URI, tt.wantScheme+"://admin:")
			assert.Contains(t, details.URI, ":"+tt.wantPort+"/")
		})
	}
}

func TestTLSEnabled(t *testing.T) {
	assert.False(t, tlsEnabled(newTestContext(t, newTestInstance("db", "ns", ""))))
	assert.False(t, tlsEnabled(newTestContext(t, newTestInstance("db", "ns", `{"tls":{"enabled":"disabled"}}`))))
	assert.True(t, tlsEnabled(newTestContext(t, newTestInstance("db", "ns", `{"tls":{"enabled":"enabled"}}`))))
}

func TestEnsureIssuerOverride(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""))

	ref, err := ensureIssuer(c, &components.IssuerRef{Name: "my-issuer"})
	require.NoError(t, err)
	assert.Equal(t, "my-issuer", ref.Name)
	assert.Equal(t, "Issuer", ref.Kind)
	assert.Equal(t, "cert-manager.io", ref.Group)

	// No self-signed resources are created when an override is supplied.
	issuer := &cmapi.Issuer{}
	assert.Error(t, c.Get(issuer, selfSignedIssuerName(c.Name())))
}

func TestEnsureIssuerSelfSigned(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""))

	ref, err := ensureIssuer(c, nil)
	require.NoError(t, err)
	assert.Equal(t, "db-ca", ref.Name)

	require.NoError(t, c.Get(&cmapi.Issuer{}, selfSignedIssuerName(c.Name())))
	require.NoError(t, c.Get(&cmapi.Certificate{}, caCertName(c.Name())))
	require.NoError(t, c.Get(&cmapi.Issuer{}, caIssuerName(c.Name())))
}
