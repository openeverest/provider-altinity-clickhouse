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
	"testing"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/components"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

const (
	tlsRequiredParams = `{"tls":{"enabled":"enabled"}}`
	tlsOptionalParams = `{"tls":{"enabled":"enabled","mode":"optional"}}`
)

func TestTLSMode(t *testing.T) {
	for params, want := range map[string]string{
		"": "",
		`{"tls":{"enabled":"disabled","mode":"optional"}}`: "",
		tlsRequiredParams: common.TLSModeRequired,
		`{"tls":{"enabled":"enabled","mode":"required"}}`: common.TLSModeRequired,
		tlsOptionalParams: common.TLSModeOptional,
		`{"tls":{"enabled":"enabled","mode":"Optional"}}`: common.TLSModeOptional,
	} {
		assert.Equal(t, want, tlsMode(newTestContext(t, newTestInstance("db", "ns", params))), params)
	}
}

func TestValidateTLSMode(t *testing.T) {
	err := New().Validate(newTestContext(t, newTestInstance("db", "ns", `{"tls":{"enabled":"enabled","mode":"strict"}}`)))
	assert.ErrorContains(t, err, "tls.mode")
	assert.NoError(t, New().Validate(newTestContext(t, newTestInstance("db", "ns", tlsOptionalParams))))
}

func servicePorts(tmpl chiv1.ServiceTemplate) []int32 {
	var ports []int32
	for _, p := range tmpl.Spec.Ports {
		ports = append(ports, p.Port)
	}
	return ports
}

func TestConfigureExposePorts(t *testing.T) {
	lb := &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer}
	tests := []struct {
		name     string
		service  *corev1alpha1.Service
		mode     string
		want     []int32
		headless bool
	}{
		{name: "plaintext, not exposed", mode: ""},
		{name: "plaintext, LoadBalancer", service: lb, want: []int32{8123, 9000}},
		{name: "optional, LoadBalancer", service: lb, mode: common.TLSModeOptional, want: []int32{8123, 9000, 8443, 9440}},
		{name: "required, LoadBalancer", service: lb, mode: common.TLSModeRequired, want: []int32{8443, 9440}},
		{name: "required, not exposed", mode: common.TLSModeRequired, want: []int32{8443, 9440}, headless: true},
		{name: "optional, not exposed", mode: common.TLSModeOptional, want: []int32{8123, 9000, 8443, 9440}, headless: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &chiv1.ChiSpec{}
			configureExpose(spec, tt.service, tt.mode)
			if tt.want == nil {
				assert.Nil(t, spec.Templates, "the operator's default Service applies")
				return
			}
			require.Len(t, spec.Templates.ServiceTemplates, 1)
			tmpl := spec.Templates.ServiceTemplates[0]
			assert.Equal(t, tt.want, servicePorts(tmpl))
			assert.Equal(t, common.ServiceTemplateName, spec.Defaults.Templates.ServiceTemplate)
			if tt.headless {
				// As the operator's default: clusterIP cannot change in place.
				assert.Equal(t, corev1.ClusterIPNone, tmpl.Spec.ClusterIP)
			} else {
				assert.Equal(t, tt.service.ServiceType, tmpl.Spec.Type)
			}
		})
	}
}

func TestBuildCHITLSModes(t *testing.T) {
	required, err := buildCHI(newTestContext(t, newTestInstance("db", "ns", tlsRequiredParams)), 2)
	require.NoError(t, err)
	assert.True(t, appliedTLSRequired(required))
	assert.Empty(t, required.Annotations, "the operator copies them onto the pods, restarting them")
	assert.True(t, required.Spec.Configuration.Clusters[0].Secure.Value(), "distributed queries run over TLS")
	openssl := required.Spec.Configuration.Files.Get("openssl.xml").ScalarString()
	assert.Contains(t, openssl, "<client>")
	assert.Contains(t, openssl, "<caConfig>/etc/clickhouse-server/secrets.d/ca.crt/db-server-tls/ca.crt</caConfig>")

	optional, err := buildCHI(newTestContext(t, newTestInstance("db", "ns", tlsOptionalParams)), 2)
	require.NoError(t, err)
	assert.False(t, appliedTLSRequired(optional))
	assert.True(t, optional.Spec.Configuration.Clusters[0].Secure.Value())

	plain, err := buildCHI(newTestContext(t, newTestInstance("db", "ns", "")), 2)
	require.NoError(t, err)
	assert.False(t, appliedTLSRequired(plain))
	assert.Nil(t, plain.Spec.Configuration.Clusters[0].Secure)
	assert.Nil(t, plain.Spec.Templates.ServiceTemplates)
}

func TestServerDNSNames(t *testing.T) {
	names := serverDNSNames("db", "ns", 2)
	for _, name := range []string{
		"clickhouse-db", "clickhouse-db.ns.svc.cluster.local",
		"chi-db-clickhouse-0-0", "chi-db-clickhouse-0-0.ns", "chi-db-clickhouse-0-0.ns.svc",
		"chi-db-clickhouse-0-1.ns.svc.cluster.local",
	} {
		assert.Contains(t, names, name)
	}
	assert.Contains(t, names, "*.chi-db-clickhouse-0-1.ns.svc.cluster.local", "pod names: replicas fetch parts by them")
	assert.Len(t, names, 14)
	assert.Equal(t, "clickhouse-db", names[0], "the common name")

	// The operator cuts the CHI name to 60 characters and trims separators.
	long := "a234567890b234567890c234567890d234567890e234567890f23456789-xyz"
	assert.Equal(t, []string{"chi-a234567890b234567890c234567890d234567890e234567890f23456789-clickhouse-0-0"},
		replicaServiceNames(long, 1))
}

func serverCertificate(t *testing.T, c *controller.Context) *cmapi.Certificate {
	t.Helper()
	cert := &cmapi.Certificate{}
	require.NoError(t, c.Get(cert, serverCertName("db")))
	return cert
}

func TestServerCertificateCoversReplicasAndLoadBalancer(t *testing.T) {
	instance := newTestInstance("db", "ns", tlsRequiredParams)
	instance.Spec.Topology = &corev1alpha1.TopologySpec{Type: common.TopologyReplicated}
	replicas := int32(3)
	engine := instance.Spec.Components[common.ComponentEngine]
	engine.Replicas = &replicas
	instance.Spec.Components[common.ComponentEngine] = engine
	lb := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "clickhouse-db", Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{
			{IP: "203.0.113.7"}, {Hostname: "db.example.com"},
		}}},
	}
	c := newTestContext(t, instance, lb)

	require.NoError(t, ensureServerCertificate(c, ensureIssuerRef(t, c)))
	cert := serverCertificate(t, c)
	assert.Contains(t, cert.Spec.DNSNames, "chi-db-clickhouse-0-2.ns.svc.cluster.local")
	assert.Contains(t, cert.Spec.DNSNames, "db.example.com")
	assert.Equal(t, []string{"203.0.113.7"}, cert.Spec.IPAddresses)
}

func ensureIssuerRef(t *testing.T, c *controller.Context) cmmeta.IssuerReference {
	t.Helper()
	ref, err := ensureIssuer(c, nil)
	require.NoError(t, err)
	return ref
}

func TestProviderManagedCAOutlivesServerCertificates(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", tlsRequiredParams))
	ensureIssuerRef(t, c)
	ca := &cmapi.Certificate{}
	require.NoError(t, c.Get(ca, caCertName("db")))
	require.NotNil(t, ca.Spec.Duration)
	assert.Equal(t, caDuration, ca.Spec.Duration.Duration)
}

// Only the first issuance holds the reconcile: a re-issue (new names, a
// renewal) flips the Certificate's Ready condition while the issued
// certificate keeps serving.
func TestEnsureTLSWaitsOnlyForFirstIssuance(t *testing.T) {
	instance := newTestInstance("db", "ns", tlsRequiredParams)
	err := ensureTLS(newTestContext(t, instance))
	assert.True(t, controller.IsWaitError(err), "got %v", err)

	assert.NoError(t, ensureTLS(newTestContext(t, instance, issuedCertificate())))
}

func TestOwnIssuerRemovesProviderManagedCA(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", tlsRequiredParams))
	ensureIssuerRef(t, c)
	require.NoError(t, c.Apply(&corev1.Secret{ObjectMeta: c.ObjectMeta(caSecretName("db"))}))

	_, err := ensureIssuer(c, &components.IssuerRef{Name: "corp-ca", Kind: "ClusterIssuer"})
	require.NoError(t, err)
	for _, obj := range []client.Object{&cmapi.Certificate{}, &cmapi.Issuer{}} {
		found, err := getOptional(c, obj, caCertName("db"))
		require.NoError(t, err)
		assert.False(t, found, "%T", obj)
	}
	found, err := getOptional(c, &corev1.Secret{}, caSecretName("db"))
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCleanupTLSAfterDisabling(t *testing.T) {
	mounting := completedCHI()
	mounting.Spec.Configuration = &chiv1.Configuration{Files: buildTLSFiles(serverSecretName("db"))}

	c := newTestContext(t, newTestInstance("db", "ns", ""), issuedCertificate())
	require.NoError(t, cleanupTLS(c, mounting))
	found, err := getOptional(c, &corev1.Secret{}, serverSecretName("db"))
	require.NoError(t, err)
	assert.True(t, found, "kept while the CHI still mounts it")

	require.NoError(t, cleanupTLS(c, completedCHI()))
	found, err = getOptional(c, &corev1.Secret{}, serverSecretName("db"))
	require.NoError(t, err)
	assert.False(t, found)
}

// Right after the CHI without TLS is applied, its status still reads
// Completed while the pods mounting the certificate have yet to roll.
func TestCleanupTLSWaitsForPodsToRoll(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "chi-db-clickhouse-0-0-0", Namespace: "ns", Labels: map[string]string{common.LabelCHIName: "db"}},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name:         "server-tls",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: serverSecretName("db")}},
		}}},
	}
	c := newTestContext(t, newTestInstance("db", "ns", ""), issuedCertificate(), pod)
	require.NoError(t, cleanupTLS(c, completedCHI()))
	found, err := getOptional(c, &corev1.Secret{}, serverSecretName("db"))
	require.NoError(t, err)
	assert.True(t, found)
}

func TestSyncCleansUpTLSWhenDisabled(t *testing.T) {
	_, c := syncStandalone(t, newTestInstance("db", "ns", ""), completedCHI(), issuedCertificate())
	found, err := getOptional(c, &corev1.Secret{}, serverSecretName("db"))
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCleanupDeletesCertificatesWithTLSOff(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""), issuedCertificate())
	require.NoError(t, New().Cleanup(c))
	found, err := getOptional(c, &corev1.Secret{}, serverSecretName("db"))
	require.NoError(t, err)
	assert.False(t, found)
}

func TestConnectionDetailsCarryCA(t *testing.T) {
	chi := completedCHI()
	details := buildConnectionDetails(newTestContext(t, newTestInstance("db", "ns", tlsRequiredParams), issuedCertificate()), chi)
	assert.Equal(t, "ca", details.AdditionalProperties[common.ConnectionKeyCACert])
	assert.Equal(t, "8443", details.Port)

	plain := buildConnectionDetails(newTestContext(t, newTestInstance("db", "ns", ""), issuedCertificate()), chi)
	assert.NotContains(t, plain.AdditionalProperties, common.ConnectionKeyCACert)
}

// withTLSMode renders chi's client Service as for mode.
func withTLSMode(chi *chiv1.ClickHouseInstallation, mode string) *chiv1.ClickHouseInstallation {
	configureExpose(&chi.Spec, nil, mode)
	return chi
}

func plaintextPolicyIn(t *testing.T, c *controller.Context) *networkingv1.NetworkPolicy {
	t.Helper()
	policy := &networkingv1.NetworkPolicy{}
	found, err := getOptional(c, policy, "db"+common.PlaintextPolicySuffix)
	require.NoError(t, err)
	if !found {
		return nil
	}
	return policy
}

func TestPlaintextPolicyFollowsAppliedCHI(t *testing.T) {
	t.Setenv(common.EnvOperatorNamespace, "clickhouse-provider")
	t.Setenv(common.EnvOperatorPodLabels, "app.kubernetes.io/name=operator,app.kubernetes.io/instance=rel")
	c := newTestContext(t, newTestInstance("db", "ns", tlsRequiredParams))

	// Not applied yet (e.g. held for maintenance): plaintext stays reachable.
	require.NoError(t, reconcilePlaintextPolicy(c, completedCHI()))
	assert.Nil(t, plaintextPolicyIn(t, c))

	require.NoError(t, reconcilePlaintextPolicy(c, withTLSMode(completedCHI(), common.TLSModeRequired)))
	policy := plaintextPolicyIn(t, c)
	require.NotNil(t, policy)
	assert.Equal(t, map[string]string{common.LabelCHIName: "db"}, policy.Spec.PodSelector.MatchLabels)
	require.Len(t, policy.Spec.Ingress, 2)

	open := policy.Spec.Ingress[0]
	assert.Empty(t, open.From, "anyone reaches the secure ports and metrics")
	assert.Equal(t, []int32{8443, 9440, 9363}, policyPorts(open.Ports))

	restricted := policy.Spec.Ingress[1]
	assert.Equal(t, []int32{8123, 9000, 9009, 9010}, policyPorts(restricted.Ports))
	require.Len(t, restricted.From, 2)
	assert.Equal(t, map[string]string{common.LabelCHIName: "db"}, restricted.From[0].PodSelector.MatchLabels)
	assert.Nil(t, restricted.From[0].NamespaceSelector, "the Instance's own namespace")
	assert.Equal(t, map[string]string{corev1.LabelMetadataName: "clickhouse-provider"}, restricted.From[1].NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "operator", "app.kubernetes.io/instance": "rel"}, restricted.From[1].PodSelector.MatchLabels)

	// Back to optional: the policy goes away with the applied CHI.
	require.NoError(t, reconcilePlaintextPolicy(c, withTLSMode(completedCHI(), common.TLSModeOptional)))
	assert.Nil(t, plaintextPolicyIn(t, c))
}

func policyPorts(ports []networkingv1.NetworkPolicyPort) []int32 {
	var out []int32
	for _, p := range ports {
		out = append(out, p.Port.IntVal)
		if p.Port.Type != intstr.Int {
			panic("named port")
		}
	}
	return out
}

func TestOperatorPeerDefaults(t *testing.T) {
	t.Setenv(common.EnvOperatorNamespace, "ops")
	t.Setenv(common.EnvOperatorPodLabels, "")
	peer, err := operatorPeer()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "operator"}, peer.PodSelector.MatchLabels)

	t.Setenv(common.EnvOperatorPodLabels, "not a selector")
	_, err = operatorPeer()
	assert.Error(t, err)
}

func TestSyncCreatesPlaintextPolicyForRequiredTLS(t *testing.T) {
	t.Setenv(common.EnvOperatorNamespace, "clickhouse-provider")
	_, c := syncStandalone(t, newTestInstance("db", "ns", tlsRequiredParams),
		withTLSMode(completedCHI(), common.TLSModeRequired), issuedCertificate())
	assert.NotNil(t, plaintextPolicyIn(t, c))
}

func TestInstanceForClientService(t *testing.T) {
	svc := func(name string, labels map[string]string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels}}
	}
	chiLabels := map[string]string{common.LabelCHIName: "db", labelOperatorService: operatorServiceCR}

	reqs := instanceForClientService(context.Background(), svc("clickhouse-db", chiLabels))
	require.Len(t, reqs, 1)
	assert.Equal(t, "ns", reqs[0].Namespace)
	assert.Equal(t, "db", reqs[0].Name)

	replicaLabels := map[string]string{common.LabelCHIName: "db", labelOperatorService: "host"}
	assert.Empty(t, instanceForClientService(context.Background(), svc("chi-db-clickhouse-0-0", replicaLabels)))
	assert.Empty(t, instanceForClientService(context.Background(), svc("unrelated", nil)))
}

func TestLoadBalancerAddressChanged(t *testing.T) {
	p := loadBalancerAddressChanged()
	lb := func(ip string) *corev1.Service {
		s := &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}
		if ip != "" {
			s.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ip}}
		}
		return s
	}
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: lb(""), ObjectNew: lb("203.0.113.7")}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: lb("203.0.113.7"), ObjectNew: lb("203.0.113.7")}))
	clusterIP := &corev1.Service{}
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: clusterIP, ObjectNew: clusterIP.DeepCopy()}))
	assert.True(t, p.Create(event.CreateEvent{Object: lb("")}))
	assert.False(t, p.Create(event.CreateEvent{Object: clusterIP}))
}

// The runtime's scheme holds only core/v1 and its own types besides what the
// provider registers; a watched type missing from it stops the controller.
func TestWatchedTypesAreRegistered(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	p := New()
	for _, add := range p.SchemeFuncs {
		require.NoError(t, add(scheme))
	}
	for _, w := range p.WatchConfigs {
		_, _, err := scheme.ObjectKinds(w.Object)
		assert.NoError(t, err, "%T", w.Object)
	}
	// Read through the cached client, which needs them registered too.
	for _, obj := range []runtime.Object{&storagev1.StorageClass{}, &networkingv1.NetworkPolicy{}} {
		_, _, err := scheme.ObjectKinds(obj)
		assert.NoError(t, err, "%T", obj)
	}
}
