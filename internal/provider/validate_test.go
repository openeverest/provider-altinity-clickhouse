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
	"cmp"
	"testing"

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

const (
	providerSpecPath  = "../../charts/provider-altinity-clickhouse/generated/provider-spec.yaml"
	standaloneFixture = "testdata/validate/standalone.yaml"
	replicatedFixture = "testdata/validate/replicated.yaml"
)

// deployedCHI renders the CHI the provider would have applied for instance,
// standing in for the live object the reference rules compare against.
func deployedCHI(t *testing.T, instance *corev1alpha1.Instance, replicas int) *chiv1.ClickHouseInstallation {
	t.Helper()
	chi, err := buildCHI(newTestContext(t, instance.DeepCopy()), replicas)
	require.NoError(t, err)
	return chi
}

// withEngine returns a copy of instance with mutate applied to its engine.
func withEngine(instance *corev1alpha1.Instance, mutate func(*corev1alpha1.ComponentSpec)) *corev1alpha1.Instance {
	out := instance.DeepCopy()
	engine := out.Spec.Components[common.ComponentEngine]
	mutate(&engine)
	out.Spec.Components[common.ComponentEngine] = engine
	return out
}

func storage(size string, storageClass *string) func(*corev1alpha1.ComponentSpec) {
	return func(e *corev1alpha1.ComponentSpec) {
		e.Storage = &corev1alpha1.Storage{Size: resource.MustParse(size), StorageClass: storageClass}
	}
}

func image(img string) func(*corev1alpha1.ComponentSpec) {
	return func(e *corev1alpha1.ComponentSpec) { e.Image = img }
}

// component applies mutate to the named component of the Instance.
func component(name string, mutate func(*corev1alpha1.ComponentSpec)) func(*corev1alpha1.Instance) {
	return func(in *corev1alpha1.Instance) {
		c := in.Spec.Components[name]
		mutate(&c)
		in.Spec.Components[name] = c
	}
}

// engineParameters sets the engine's parameters to raw.
func engineParameters(raw string) func(*corev1alpha1.Instance) {
	return component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
		c.Parameters = &runtime.RawExtension{Raw: []byte(raw)}
	})
}

func replicas(n int32) func(*corev1alpha1.ComponentSpec) {
	return func(c *corev1alpha1.ComponentSpec) { c.Replicas = &n }
}

func addKeeper(in *corev1alpha1.Instance) {
	in.Spec.Components[common.ComponentKeeper] = corev1alpha1.ComponentSpec{}
}

// TestValidate runs the checks the runtime runs before every Sync: the
// schemas, supportedFields and catalog the provider publishes, then Validate.
func TestValidate(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)

	tests := []struct {
		name    string
		fixture string
		mutate  func(*corev1alpha1.Instance)
		want    []string
	}{
		{name: "valid standalone", fixture: standaloneFixture},
		{name: "valid replicated", fixture: replicatedFixture},
		{
			name:    "no topology type",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { in.Spec.Topology = nil },
		},
		{
			name:    "missing engine without a topology type",
			fixture: standaloneFixture,
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Topology = nil
				delete(in.Spec.Components, common.ComponentEngine)
			},
			want: []string{"spec.components[engine]"},
		},
		{
			name:    "unknown version",
			fixture: standaloneFixture,
			mutate:  func(in *corev1alpha1.Instance) { in.Spec.Version = "23.8" },
			want:    []string{"spec.version"},
		},
		{
			name:    "engine limits below the minimums",
			fixture: standaloneFixture,
			mutate: component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
				c.Resources.Limits = list("500m", "512Mi")
			}),
			want: []string{
				"spec.components[engine].resources.limits[cpu]",
				"spec.components[engine].resources.limits[memory]",
			},
		},
		{
			name:    "engine request above its limit",
			fixture: standaloneFixture,
			mutate: component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
				c.Resources.Requests = list("", "8Gi")
			}),
			want: []string{"spec.components[engine].resources.requests[memory]"},
		},
		{
			name:    "one replica in the replicated topology",
			fixture: replicatedFixture,
			mutate:  component(common.ComponentEngine, replicas(1)),
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "three replicas in the replicated topology",
			fixture: replicatedFixture,
			mutate:  component(common.ComponentEngine, replicas(3)),
		},
		{
			name:    "three replicas in the standalone topology",
			fixture: standaloneFixture,
			mutate:  component(common.ComponentEngine, replicas(3)),
			want:    []string{"spec.components[engine].replicas"},
		},
		{
			name:    "three replicas without a topology type",
			fixture: standaloneFixture,
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Topology = nil
				component(common.ComponentEngine, replicas(3))(in)
			},
			want: []string{"spec.components[engine].replicas"},
		},
		{
			name:    "Retain storage reclaim policy",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"storageReclaimPolicy":"Retain"}`),
		},
		{
			name:    "Delete storage reclaim policy",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"storageReclaimPolicy":"Delete"}`),
		},
		{
			name:    "storage reclaim policy in lower case",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"storageReclaimPolicy":"delete"}`),
		},
		{
			name:    "unknown storage reclaim policy",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"storageReclaimPolicy":"Recycle"}`),
			want:    []string{"spec.components[engine].parameters.storageReclaimPolicy"},
		},
		{
			name:    "optional TLS",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"tls":{"enabled":"enabled","mode":"Optional"}}`),
		},
		{
			name:    "unknown TLS mode",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"tls":{"enabled":"enabled","mode":"strict"}}`),
			want:    []string{"spec.components[engine].parameters.tls.mode"},
		},
		{
			name:    "issuer without a name",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"tls":{"enabled":"enabled","issuerRef":{"kind":"ClusterIssuer"}}}`),
			want:    []string{"spec.components[engine].parameters.tls.issuerRef.name"},
		},
		{
			name:    "server settings",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"configuration":"max_concurrent_queries: 200\nlogger/level: information"}`),
		},
		{
			name:    "configuration setting a port the operator manages",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"configuration":"http_port: 8124"}`),
			want:    []string{"spec.components[engine].parameters.configuration"},
		},
		{
			name:    "unknown engine parameter",
			fixture: standaloneFixture,
			mutate:  engineParameters(`{"podmonitor":"enabled"}`),
			want:    []string{"spec.components[engine].parameters.podmonitor"},
		},
		{
			name:    "unsupported service type",
			fixture: standaloneFixture,
			mutate: component(common.ComponentEngine, func(c *corev1alpha1.ComponentSpec) {
				c.Service = &corev1alpha1.Service{ServiceType: corev1.ServiceTypeExternalName}
			}),
			want: []string{"spec.components[engine].service.serviceType"},
		},
		{
			name:    "keeper without a topology type",
			fixture: standaloneFixture,
			mutate: func(in *corev1alpha1.Instance) {
				in.Spec.Topology = nil
				addKeeper(in)
			},
			want: []string{"spec.components[keeper]"},
		},
		{
			name:    "keeper in the standalone topology",
			fixture: standaloneFixture,
			mutate:  addKeeper,
			want:    []string{"spec.components[keeper]"},
		},
		{
			name:    "replicated topology without keeper",
			fixture: replicatedFixture,
			mutate:  func(in *corev1alpha1.Instance) { delete(in.Spec.Components, common.ComponentKeeper) },
			want:    []string{"spec.components[keeper]"},
		},
		{
			name:    "three Keeper replicas",
			fixture: replicatedFixture,
			mutate:  component(common.ComponentKeeper, replicas(3)),
		},
		{
			name:    "five Keeper replicas",
			fixture: replicatedFixture,
			mutate:  component(common.ComponentKeeper, replicas(5)),
			want:    []string{"spec.components[keeper].replicas"},
		},
		{
			name:    "Keeper request above its limit",
			fixture: replicatedFixture,
			mutate: component(common.ComponentKeeper, func(c *corev1alpha1.ComponentSpec) {
				c.Resources.Requests = list("1", "")
			}),
			want: []string{"spec.components[keeper].resources.requests[cpu]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := validationtest.Load(t, tt.fixture)
			if tt.mutate != nil {
				tt.mutate(in)
			}

			res := v.Run(in, nil)

			var got []string
			for _, err := range res.Errors {
				got = append(got, err.Field)
			}
			assert.Equal(t, tt.want, got, "errors: %v", res.Errors)
		})
	}
}

func TestValidateConfiguration(t *testing.T) {
	t.Parallel()
	for cfg, wantErr := range map[string]string{
		"":                                     "",
		"max_concurrent_queries: 200":          "",
		"logger/level: information":            "",
		"prometheus/metrics: false":            "",
		"max_concurrent_queries: [unclosed":    "not valid YAML",
		"http_port: 8124":                      "must not set http_port",
		"tcp_port_secure: 9441":                "must not set tcp_port_secure",
		"prometheus/port: 9999":                "must not set prometheus/port",
		"zookeeper/node/host: zk":              "must not set zookeeper/node/host",
		"openSSL/server/verificationMode: no":  "must not set openSSL/server/verificationMode",
		"remote_servers: x":                    "must not set remote_servers",
		"interserver_http_credentials/user: x": "must not set interserver_http_credentials/user",
	} {
		err := validateConfiguration(cfg)
		if wantErr == "" {
			assert.NoError(t, err, cfg)
		} else {
			assert.ErrorContains(t, err, wantErr, cfg)
		}
	}
}

// TestValidateTransitions runs the pipeline as admission does on an update,
// with the stored Instance as the previous one.
func TestValidateTransitions(t *testing.T) {
	t.Parallel()
	v := validationtest.New(t, New(), providerSpecPath)
	version := func(name string) func(*corev1alpha1.Instance) {
		return func(in *corev1alpha1.Instance) { in.Spec.Version = name }
	}

	tests := []struct {
		name    string
		fixture string
		// phase of the stored Instance, Ready when empty.
		phase  corev1alpha1.InstancePhase
		before func(*corev1alpha1.Instance)
		after  func(*corev1alpha1.Instance)
		want   []string
	}{
		{name: "standalone unchanged", fixture: standaloneFixture},
		{name: "replicated unchanged", fixture: replicatedFixture},
		{
			name:    "engine storage grows",
			fixture: standaloneFixture,
			after:   component(common.ComponentEngine, storage("100Gi", nil)),
		},
		{
			name:    "engine storage shrinks",
			fixture: standaloneFixture,
			after:   component(common.ComponentEngine, storage("20Gi", nil)),
			want:    []string{"spec.components[engine].storage.size"},
		},
		{
			name:    "engine storage shrinks on a failed cluster",
			fixture: standaloneFixture,
			phase:   corev1alpha1.InstancePhaseFailed,
			after:   component(common.ComponentEngine, storage("20Gi", nil)),
			want:    []string{"spec.components[engine].storage.size"},
		},
		{
			name:    "engine storage shrinks before the cluster is created",
			fixture: standaloneFixture,
			phase:   corev1alpha1.InstancePhasePending,
			after:   component(common.ComponentEngine, storage("20Gi", nil)),
		},
		{
			name:    "engine storage class changes",
			fixture: standaloneFixture,
			before:  component(common.ComponentEngine, storage("25Gi", new("fast"))),
			after:   component(common.ComponentEngine, storage("25Gi", new("slow"))),
			want:    []string{"spec.components[engine].storage.storageClass"},
		},
		{
			name:    "engine storage class kept",
			fixture: standaloneFixture,
			before:  component(common.ComponentEngine, storage("25Gi", new("fast"))),
		},
		{
			name:    "engine storage class unset",
			fixture: standaloneFixture,
			before:  component(common.ComponentEngine, storage("25Gi", new("fast"))),
			after:   component(common.ComponentEngine, storage("25Gi", nil)),
			want:    []string{"spec.components[engine].storage.storageClass"},
		},
		{
			name:    "engine storage class set on the cluster default",
			fixture: standaloneFixture,
			after:   component(common.ComponentEngine, storage("25Gi", new("fast"))),
			want:    []string{"spec.components[engine].storage.storageClass"},
		},
		{
			name:    "replicated to standalone",
			fixture: replicatedFixture,
			after: func(in *corev1alpha1.Instance) {
				in.Spec.Topology.Type = common.TopologyStandalone
				delete(in.Spec.Components, common.ComponentKeeper)
				component(common.ComponentEngine, replicas(1))(in)
			},
			want: []string{"spec.topology.type"},
		},
		{
			name:    "replicated to standalone while provisioning",
			fixture: replicatedFixture,
			phase:   corev1alpha1.InstancePhaseProvisioning,
			after: func(in *corev1alpha1.Instance) {
				in.Spec.Topology.Type = common.TopologyStandalone
				delete(in.Spec.Components, common.ComponentKeeper)
				component(common.ComponentEngine, replicas(1))(in)
			},
			want: []string{"spec.topology.type"},
		},
		{
			name:    "standalone to replicated",
			fixture: standaloneFixture,
			after: func(in *corev1alpha1.Instance) {
				in.Spec.Topology.Type = common.TopologyReplicated
				addKeeper(in)
				component(common.ComponentEngine, replicas(2))(in)
			},
		},
		{
			name:    "ClickHouse upgraded",
			fixture: standaloneFixture,
			before:  version("26.3"),
			after:   version("26.8"),
		},
		{
			name:    "ClickHouse downgraded",
			fixture: standaloneFixture,
			before:  version("26.8"),
			after:   version("26.3"),
			want:    []string{"spec.version"},
		},
		{
			name:    "ClickHouse and Keeper downgraded",
			fixture: replicatedFixture,
			before:  version("26.8"),
			after:   version("26.3"),
			want:    []string{"spec.version", "spec.version"},
		},
		{
			name:    "ClickHouse image downgraded",
			fixture: standaloneFixture,
			before:  component(common.ComponentEngine, image("clickhouse/clickhouse-server:25.8.2")),
			after:   component(common.ComponentEngine, image("clickhouse/clickhouse-server:25.3.5")),
			want:    []string{"spec.components[engine].image"},
		},
		{
			name:    "Keeper storage grows",
			fixture: replicatedFixture,
			after:   component(common.ComponentKeeper, storage("20Gi", nil)),
		},
		{
			name:    "Keeper storage shrinks",
			fixture: replicatedFixture,
			after:   component(common.ComponentKeeper, storage("5Gi", nil)),
			want:    []string{"spec.components[keeper].storage.size"},
		},
		{
			name:    "Keeper storage class unset",
			fixture: replicatedFixture,
			before:  component(common.ComponentKeeper, storage("10Gi", new("fast"))),
			after:   component(common.ComponentKeeper, storage("10Gi", nil)),
		},
		{
			name:    "Keeper storage class set to the engine's",
			fixture: replicatedFixture,
			before:  component(common.ComponentEngine, storage("50Gi", new("fast"))),
			after:   component(common.ComponentKeeper, storage("10Gi", new("fast"))),
		},
		{
			name:    "Keeper storage class set to another than the engine's",
			fixture: replicatedFixture,
			before:  component(common.ComponentEngine, storage("50Gi", new("fast"))),
			after:   component(common.ComponentKeeper, storage("10Gi", new("slow"))),
			want:    []string{"spec.components[keeper].storage.storageClass"},
		},
		{
			name:    "Keeper back on the version bundle from a newer image",
			fixture: replicatedFixture,
			before:  component(common.ComponentKeeper, image("clickhouse/clickhouse-keeper:27.3.1")),
			after:   component(common.ComponentKeeper, image("")),
			want:    []string{"spec.version"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old := validationtest.Load(t, tt.fixture)
			if tt.before != nil {
				tt.before(old)
			}
			old.Status.Phase = cmp.Or(tt.phase, corev1alpha1.InstancePhaseReady)
			in := old.DeepCopy()
			if tt.after != nil {
				tt.after(in)
			}

			res := v.Run(in, old)

			var got []string
			for _, err := range res.Errors {
				got = append(got, err.Field)
			}
			assert.Equal(t, tt.want, got, "errors: %v", res.Errors)
		})
	}
}

// Images are compared by the release segments both tags have.
func TestValidateDowngrade(t *testing.T) {
	t.Parallel()
	for img, wantErr := range map[string]bool{
		"clickhouse/clickhouse-server:25.3.5":         false, // same release, shorter tag
		"clickhouse/clickhouse-server:25.8.2":         false,
		"clickhouse/clickhouse-server:25.3.5-alpine":  false,
		"clickhouse/clickhouse-server:latest":         false, // not a version: not compared
		"registry:5000/clickhouse/server:25.3.6":      false,
		"clickhouse/clickhouse-server:24.8.14":        true,
		"clickhouse/clickhouse-server:25.3.4":         true,
		"registry:5000/clickhouse/server:25.2-alpine": true,
	} {
		errs := validateDowngrade(field.NewPath("spec", "version"), "clickhouse/clickhouse-server:25.3.5.42", img)
		assert.Equal(t, wantErr, len(errs) > 0, img)
	}
}

func storageClass(name string, expandable *bool) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "example.com/csi", AllowVolumeExpansion: expandable}
}

func volume(name, class string, labels map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
	}
}

// TestValidateReferences checks the rules against the deployed cluster, which
// also catch what changes without an update of the Instance, such as the
// version a provider release runs by default.
func TestValidateReferences(t *testing.T) {
	yes, no := true, false
	base := withEngine(newTestInstance("db", "ns", ""), storage("50Gi", nil))
	grown := withEngine(base, storage("100Gi", nil))
	live := deployedCHI(t, base, 1)
	newerCHI := deployedCHI(t, withEngine(base, image("clickhouse/clickhouse-server:27.1.1")), 1)
	pvc := volume("data-chi-db-clickhouse-0-0-0", "local-path", map[string]string{common.LabelCHIName: "db"})
	other := volume("data-chi-other-clickhouse-0-0-0", "fixed", map[string]string{common.LabelCHIName: "other"})

	// A Keeper on the cluster default, as before the keeper component existed.
	replicated := withKeeper(newReplicatedTestInstance(""), storage("20Gi", nil))
	liveKeeper, _ := mustBuildCHK(t, replicated)
	newerCHK, _ := mustBuildCHK(t, withKeeper(replicated, image("clickhouse/clickhouse-keeper:27.3.1")))
	keeperPVC := volume("keeper-data-chk-db-keeper-keeper-0-0-0", "local-path", map[string]string{common.LabelCHKName: "db-keeper"})

	tests := []struct {
		name     string
		instance *corev1alpha1.Instance
		objects  []client.Object
		want     []string
	}{
		{name: "no deployed cluster", instance: grown},
		{name: "user secret not found yet", instance: withUserSecretRef(base, "seed")},
		{
			name:     "engine storage unchanged",
			instance: base,
			objects:  []client.Object{live, pvc, storageClass("local-path", nil)},
		},
		{
			name:     "engine storage grows on a StorageClass without volume expansion",
			instance: grown,
			objects:  []client.Object{live, pvc, storageClass("local-path", nil)},
			want:     []string{"spec.components[engine].storage.size"},
		},
		{
			name:     "engine storage grows on a StorageClass that disallows volume expansion",
			instance: grown,
			objects:  []client.Object{live, pvc, storageClass("local-path", &no)},
			want:     []string{"spec.components[engine].storage.size"},
		},
		{
			name:     "engine storage grows on a StorageClass that allows volume expansion",
			instance: grown,
			objects:  []client.Object{live, pvc, storageClass("local-path", &yes)},
		},
		{
			name:     "engine storage grows on a StorageClass that is gone",
			instance: grown,
			objects:  []client.Object{live, pvc},
		},
		{
			name:     "another Instance's volumes",
			instance: grown,
			objects:  []client.Object{live, other, storageClass("fixed", nil)},
		},
		{
			name:     "ClickHouse below the running version",
			instance: withEngine(base, image("")),
			objects:  []client.Object{newerCHI},
			want:     []string{"spec.version"},
		},
		{
			name:     "Keeper storage grows on a StorageClass without volume expansion",
			instance: withKeeper(replicated, storage("30Gi", nil)),
			objects:  []client.Object{liveKeeper, keeperPVC, storageClass("local-path", nil)},
			want:     []string{"spec.components[keeper].storage.size"},
		},
		{
			name:     "Keeper keeps its StorageClass when the engine is on another",
			instance: withEngine(replicated, storage("25Gi", new("fast"))),
			objects:  []client.Object{liveKeeper},
		},
		{
			name:     "Keeper moved off the cluster default",
			instance: withEngine(withKeeper(replicated, storage("20Gi", new("fast"))), storage("25Gi", new("fast"))),
			objects:  []client.Object{liveKeeper},
			want:     []string{"spec.components[keeper].storage.storageClass"},
		},
		{
			name:     "Keeper below the running version",
			instance: replicated,
			objects:  []client.Object{newerCHK},
			want:     []string{"spec.version"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs, err := New().ValidateReferences(newTestContext(t, tt.instance.DeepCopy(), tt.objects...))
			require.NoError(t, err)

			var got []string
			for _, e := range errs {
				got = append(got, e.Field)
			}
			assert.Equal(t, tt.want, got, "errors: %v", errs)
		})
	}
}
