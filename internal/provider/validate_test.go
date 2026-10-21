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
	"encoding/json"
	"testing"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// deployedCHI renders the CHI the provider would have applied for instance,
// standing in for the live object the transition rules compare against.
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

func validate(t *testing.T, desired *corev1alpha1.Instance, live ...client.Object) error {
	t.Helper()
	return New().Validate(newTestContext(t, desired, live...))
}

func TestValidateWithoutDeployedClusterSkipsTransitionRules(t *testing.T) {
	assert.NoError(t, validate(t, withEngine(newTestInstance("db", "ns", ""), storage("10Gi", nil))))
}

func TestValidateStorageSize(t *testing.T) {
	base := withEngine(newTestInstance("db", "ns", ""), storage("50Gi", nil))
	live := deployedCHI(t, base, 1)

	assert.NoError(t, validate(t, base, live))
	assert.NoError(t, validate(t, withEngine(base, storage("100Gi", nil)), live))
	assert.ErrorContains(t, validate(t, withEngine(base, storage("20Gi", nil)), live), "cannot shrink from 50Gi to 20Gi")
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

// Growing volumes whose StorageClass cannot expand them would leave the
// operator's PVC resize refused and the change stuck.
func TestValidateStorageGrowthNeedsExpandableClass(t *testing.T) {
	base := withEngine(newTestInstance("db", "ns", ""), storage("50Gi", nil))
	live := deployedCHI(t, base, 1)
	pvc := volume("data-chi-db-clickhouse-0-0-0", "local-path", map[string]string{common.LabelCHIName: "db"})
	grow := withEngine(base, storage("100Gi", nil))
	yes, no := true, false

	assert.ErrorContains(t, validate(t, grow, live, pvc, storageClass("local-path", nil)),
		"engine storage cannot grow: the local-path StorageClass does not allow volume expansion")
	assert.ErrorContains(t, validate(t, grow, live, pvc, storageClass("local-path", &no)), "does not allow volume expansion")
	assert.NoError(t, validate(t, grow, live, pvc, storageClass("local-path", &yes)))
	assert.NoError(t, validate(t, grow, live, pvc), "a class that is gone is not judged")
	assert.NoError(t, validate(t, base, live, pvc, storageClass("local-path", nil)), "unchanged size needs no expansion")

	other := volume("data-chi-other-clickhouse-0-0-0", "fixed", map[string]string{common.LabelCHIName: "other"})
	assert.NoError(t, validate(t, grow, live, other, storageClass("fixed", nil)), "another Instance's volumes")
}

func TestValidateKeeperStorageGrowthNeedsExpandableClass(t *testing.T) {
	base := newReplicatedTestInstance("")
	keeperSize := func(size string) func(*corev1alpha1.ComponentSpec) {
		return func(k *corev1alpha1.ComponentSpec) {
			k.Storage = &corev1alpha1.Storage{Size: resource.MustParse(size)}
		}
	}
	live, _ := mustBuildCHK(t, withKeeper(base, keeperSize("20Gi")))
	pvc := volume("keeper-data-chk-db-keeper-keeper-0-0-0", "local-path", map[string]string{common.LabelCHKName: "db-keeper"})

	assert.ErrorContains(t, validate(t, withKeeper(base, keeperSize("30Gi")), live, pvc, storageClass("local-path", nil)),
		"keeper storage cannot grow: the local-path StorageClass does not allow volume expansion")
}

func TestValidateStorageClass(t *testing.T) {
	fast, slow := "fast", "slow"
	withFast := withEngine(newTestInstance("db", "ns", ""), storage("50Gi", &fast))
	live := deployedCHI(t, withFast, 1)

	assert.NoError(t, validate(t, withFast, live))
	assert.ErrorContains(t, validate(t, withEngine(withFast, storage("50Gi", &slow)), live), `from "fast" to "slow"`)
	assert.ErrorContains(t, validate(t, withEngine(withFast, storage("50Gi", nil)), live), "to the cluster default")

	defaultClass := withEngine(newTestInstance("db", "ns", ""), storage("50Gi", nil))
	assert.ErrorContains(t, validate(t, withFast, deployedCHI(t, defaultClass, 1)), `from the cluster default to "fast"`)
}

func TestValidateTopologyDowngrade(t *testing.T) {
	replicated := newReplicatedTestInstance("")
	standalone := newTestInstance("db", "ns", "")

	assert.NoError(t, validate(t, replicated, deployedCHI(t, replicated, 2)))
	assert.ErrorContains(t, validate(t, standalone, deployedCHI(t, replicated, 2)), "cannot be changed from replicated to standalone")

	// A Keeper alone also marks the cluster as replicated (e.g. mid-provisioning).
	keeper := &chkv1.ClickHouseKeeperInstallation{ObjectMeta: metav1.ObjectMeta{Name: "db-keeper", Namespace: "ns"}}
	assert.Error(t, validate(t, standalone, deployedCHI(t, standalone, 1), keeper))

	// Standalone to replicated stays allowed (#4).
	assert.NoError(t, validate(t, replicated, deployedCHI(t, standalone, 1)))
}

func TestValidateVersionDowngrade(t *testing.T) {
	running := withEngine(newTestInstance("db", "ns", ""), image("clickhouse/clickhouse-server:25.3.5.42"))
	live := deployedCHI(t, running, 1)

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
		err := validate(t, withEngine(running, image(img)), live)
		if wantErr {
			assert.ErrorContains(t, err, "cannot be downgraded", img)
		} else {
			assert.NoError(t, err, img)
		}
	}
}

func TestValidateStandaloneReplicas(t *testing.T) {
	replicas := func(n int32) func(*corev1alpha1.ComponentSpec) {
		return func(e *corev1alpha1.ComponentSpec) { e.Replicas = &n }
	}
	assert.NoError(t, validate(t, withEngine(newTestInstance("db", "ns", ""), replicas(1))))
	assert.ErrorContains(t, validate(t, withEngine(newTestInstance("db", "ns", ""), replicas(3))), "single replica")
	assert.NoError(t, validate(t, withEngine(newReplicatedTestInstance(""), replicas(3))))
}

func TestValidateConfiguration(t *testing.T) {
	for cfg, wantErr := range map[string]string{
		"":                                    "",
		"max_concurrent_queries: 200":         "",
		"logger/level: information":           "",
		"prometheus/metrics: false":           "",
		"max_concurrent_queries: [unclosed":   "not valid YAML",
		"http_port: 8124":                     "must not set http_port",
		"tcp_port_secure: 9441":               "must not set tcp_port_secure",
		"prometheus/port: 9999":               "must not set prometheus/port",
		"zookeeper/node/host: zk":             "must not set zookeeper/node/host",
		"openSSL/server/verificationMode: no": "must not set openSSL/server/verificationMode",
		"remote_servers: x":                   "must not set remote_servers",
	} {
		params := ""
		if cfg != "" {
			raw, err := json.Marshal(map[string]string{"configuration": cfg})
			require.NoError(t, err)
			params = string(raw)
		}
		err := validate(t, newTestInstance("db", "ns", params))
		if wantErr == "" {
			assert.NoError(t, err, cfg)
		} else {
			assert.ErrorContains(t, err, wantErr, cfg)
		}
	}
}
