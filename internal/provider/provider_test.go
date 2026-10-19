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

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	require.NoError(t, chkv1.AddToScheme(scheme))
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

func TestSyncLabelsEnginePods(t *testing.T) {
	existing := &chiv1.ClickHouseInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns"}}
	existing.EnsureStatus().Status = chiv1.StatusCompleted

	var applied []*chiv1.ClickHouseInstallation
	c := newInterceptedTestContext(t, captureCHIApply(t, &applied), newTestInstance("db", "ns", ""), existing)

	require.NoError(t, New().Sync(c))

	require.Len(t, applied, 1)
	assert.Equal(t, map[string]string{
		controller.ProviderLabel:  common.ProviderName,
		controller.InstanceLabel:  "db",
		controller.ComponentLabel: common.ComponentEngine,
	}, applied[0].Spec.Templates.PodTemplates[0].ObjectMeta.Labels)
	assert.Equal(t, []string{common.ComponentEngine}, c.LabelledComponents())
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

func TestStorageReclaimPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params string
		want   chiv1.PVCReclaimPolicy
	}{
		{name: "defaults to Retain", want: chiv1.PVCReclaimPolicyRetain},
		{name: "explicit Retain", params: `{"storageReclaimPolicy":"Retain"}`, want: chiv1.PVCReclaimPolicyRetain},
		{name: "Delete", params: `{"storageReclaimPolicy":"Delete"}`, want: chiv1.PVCReclaimPolicyDelete},
		{name: "case-insensitive", params: `{"storageReclaimPolicy":"delete"}`, want: chiv1.PVCReclaimPolicyDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestContext(t, newTestInstance("db", "ns", tc.params))

			chi, err := buildCHI(c, 2)
			require.NoError(t, err)
			require.Len(t, chi.Spec.Templates.VolumeClaimTemplates, 1)
			assert.Equal(t, tc.want, chi.Spec.Templates.VolumeClaimTemplates[0].PVCReclaimPolicy)

			chk := buildCHK(c)
			require.Len(t, chk.Spec.Templates.VolumeClaimTemplates, 1)
			assert.Equal(t, tc.want, chk.Spec.Templates.VolumeClaimTemplates[0].PVCReclaimPolicy)
		})
	}
}

func TestValidateStorageReclaimPolicy(t *testing.T) {
	for _, params := range []string{"", `{"storageReclaimPolicy":"Retain"}`, `{"storageReclaimPolicy":"Delete"}`} {
		assert.NoError(t, New().Validate(newTestContext(t, newTestInstance("db", "ns", params))), params)
	}
	err := New().Validate(newTestContext(t, newTestInstance("db", "ns", `{"storageReclaimPolicy":"Recycle"}`)))
	assert.ErrorContains(t, err, "storageReclaimPolicy")
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

func TestBuildCHIReferencesKeeper(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""))

	replicated, err := buildCHI(c, 2)
	require.NoError(t, err)
	zk := replicated.Spec.Configuration.Zookeeper
	require.NotNil(t, zk)
	// A reference, not hard-coded nodes: the operator resolves it to the
	// Keeper hosts' ready-only client Services.
	assert.Equal(t, &chiv1.KeeperRef{Name: "db-keeper"}, zk.Keeper)
	assert.Empty(t, zk.Nodes)

	standalone, err := buildCHI(c, 1)
	require.NoError(t, err)
	assert.Nil(t, standalone.Spec.Configuration.Zookeeper)
}

func newReplicatedTestInstance(params string) *corev1alpha1.Instance {
	instance := newTestInstance("db", "ns", params)
	instance.Spec.Topology = &corev1alpha1.TopologySpec{Type: common.TopologyReplicated}
	return instance
}

func newPVC(name string, labels map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels}}
}

func cleanupPVCs() []client.Object {
	return []client.Object{
		newPVC("data-chi-db-clickhouse-0-0-0", map[string]string{common.LabelCHIName: "db"}),
		newPVC("keeper-data-chk-db-keeper-keeper-0-0-0", map[string]string{common.LabelCHKName: "db-keeper"}),
		newPVC("data-chi-other-clickhouse-0-0-0", map[string]string{common.LabelCHIName: "other"}),
	}
}

func pvcExists(t *testing.T, c *controller.Context, name string) bool {
	t.Helper()
	exists, err := c.Exists(&corev1.PersistentVolumeClaim{}, name)
	require.NoError(t, err)
	return exists
}

func TestCleanupDeletesKeeperOnlyAfterClickHouseIsGone(t *testing.T) {
	// The operator's finalizer keeps the CHI around while it tears the pods down.
	chi := &chiv1.ClickHouseInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Finalizers: []string{"test/hold"}}}
	chk := &chkv1.ClickHouseKeeperInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db-keeper", Namespace: "ns"}}
	c := newTestContext(t, newReplicatedTestInstance(""), chi, chk)

	err := New().Cleanup(c)
	require.True(t, controller.IsWaitError(err), "expected a WaitError, got %v", err)

	exists, err := c.Exists(&chkv1.ClickHouseKeeperInstallation{}, "db-keeper")
	require.NoError(t, err)
	assert.True(t, exists, "Keeper must outlive ClickHouse")

	current := &chiv1.ClickHouseInstallation{}
	require.NoError(t, c.Get(current, "db"))
	current.Finalizers = nil
	require.NoError(t, c.Client().Update(c.Context(), current))

	require.NoError(t, New().Cleanup(c))
	err = c.Get(&chkv1.ClickHouseKeeperInstallation{}, "db-keeper")
	assert.True(t, apierrors.IsNotFound(err), "expected Keeper to be deleted, got %v", err)
}

func TestCleanupRetainKeepsPVCs(t *testing.T) {
	c := newTestContext(t, newReplicatedTestInstance(""), cleanupPVCs()...)

	require.NoError(t, New().Cleanup(c))

	assert.True(t, pvcExists(t, c, "data-chi-db-clickhouse-0-0-0"))
	assert.True(t, pvcExists(t, c, "keeper-data-chk-db-keeper-keeper-0-0-0"))
	assert.True(t, pvcExists(t, c, "data-chi-other-clickhouse-0-0-0"))
}

func TestCleanupDeletePurgesInstancePVCs(t *testing.T) {
	c := newTestContext(t, newReplicatedTestInstance(`{"storageReclaimPolicy":"Delete"}`), cleanupPVCs()...)

	require.NoError(t, New().Cleanup(c))

	assert.False(t, pvcExists(t, c, "data-chi-db-clickhouse-0-0-0"))
	assert.False(t, pvcExists(t, c, "keeper-data-chk-db-keeper-keeper-0-0-0"))
	assert.True(t, pvcExists(t, c, "data-chi-other-clickhouse-0-0-0"), "PVCs of other instances must be left alone")
}

func TestCleanupAppliesCurrentReclaimPolicyBeforeDeleting(t *testing.T) {
	// The CHI and the PVC still carry Delete (a policy change not yet synced, or
	// a CHI from before the policy existed) while the Instance asks for Retain.
	chi := &chiv1.ClickHouseInstallation{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Finalizers: []string{"test/hold"}},
		Spec: chiv1.ChiSpec{Templates: &chiv1.Templates{VolumeClaimTemplates: []chiv1.VolumeClaimTemplate{
			{Name: common.DataVolumeClaimTemplateName, StorageManagement: chiv1.StorageManagement{PVCReclaimPolicy: chiv1.PVCReclaimPolicyDelete}},
		}}},
	}
	pvc := newPVC("data-chi-db-clickhouse-0-0-0", map[string]string{
		common.LabelCHIName:          "db",
		common.LabelCHIReclaimPolicy: common.StorageReclaimPolicyDelete,
	})
	c := newTestContext(t, newTestInstance("db", "ns", ""), chi, pvc)

	err := New().Cleanup(c)
	require.True(t, controller.IsWaitError(err), "expected a WaitError, got %v", err)

	current := &chiv1.ClickHouseInstallation{}
	require.NoError(t, c.Get(current, "db"))
	require.Len(t, current.Spec.Templates.VolumeClaimTemplates, 1)
	assert.Equal(t, chiv1.PVCReclaimPolicyRetain, current.Spec.Templates.VolumeClaimTemplates[0].PVCReclaimPolicy)

	currentPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(currentPVC, "data-chi-db-clickhouse-0-0-0"))
	assert.Equal(t, common.StorageReclaimPolicyRetain, currentPVC.Labels[common.LabelCHIReclaimPolicy])
}
