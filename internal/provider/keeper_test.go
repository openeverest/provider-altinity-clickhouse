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
	"testing"

	chkv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// withKeeper returns a copy of instance with mutate applied to its keeper
// component, creating the component if needed.
func withKeeper(instance *corev1alpha1.Instance, mutate func(*corev1alpha1.ComponentSpec)) *corev1alpha1.Instance {
	out := instance.DeepCopy()
	keeper := out.Spec.Components[common.ComponentKeeper]
	mutate(&keeper)
	out.Spec.Components[common.ComponentKeeper] = keeper
	return out
}

func mustBuildCHK(t *testing.T, instance *corev1alpha1.Instance, live ...client.Object) (*chkv1.ClickHouseKeeperInstallation, *controller.Context) {
	t.Helper()
	c := newTestContext(t, instance, live...)
	chk, err := buildCHK(c)
	require.NoError(t, err)
	return chk, c
}

func keeperContainer(chk *chkv1.ClickHouseKeeperInstallation) corev1.Container {
	return chk.Spec.Templates.PodTemplates[0].Spec.Containers[0]
}

func TestBuildCHKDefaults(t *testing.T) {
	chk, c := mustBuildCHK(t, newReplicatedTestInstance(""))

	container := keeperContainer(chk)
	// The default bundle pins Keeper to the version Keepers have always run.
	assert.Equal(t, "clickhouse/clickhouse-keeper:25.3.5", container.Image)
	assert.Equal(t, resource.MustParse("500m"), container.Resources.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("1Gi"), container.Resources.Limits[corev1.ResourceMemory])
	assert.Equal(t, container.Resources.Limits, container.Resources.Requests)

	vct := chk.Spec.Templates.VolumeClaimTemplates[0].Spec
	assert.Equal(t, resource.MustParse("10Gi"), vct.Resources.Requests[corev1.ResourceStorage])
	assert.Nil(t, vct.StorageClassName)

	assert.Equal(t, common.ComponentKeeper, chk.Spec.Templates.PodTemplates[0].ObjectMeta.Labels[controller.ComponentLabel])
	assert.Contains(t, c.LabelledComponents(), common.ComponentKeeper)
}

func TestBuildCHKFromKeeperComponent(t *testing.T) {
	ssd := "ssd"
	instance := withKeeper(newReplicatedTestInstance(""), func(k *corev1alpha1.ComponentSpec) {
		k.Image = "registry.example/keeper:25.8.1"
		k.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
		}}
		k.Storage = &corev1alpha1.Storage{Size: resource.MustParse("20Gi"), StorageClass: &ssd}
	})
	chk, _ := mustBuildCHK(t, instance)

	container := keeperContainer(chk)
	assert.Equal(t, "registry.example/keeper:25.8.1", container.Image)
	assert.Equal(t, resource.MustParse("2"), container.Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("4Gi"), container.Resources.Requests[corev1.ResourceMemory])
	vct := chk.Spec.Templates.VolumeClaimTemplates[0].Spec
	assert.Equal(t, resource.MustParse("20Gi"), vct.Resources.Requests[corev1.ResourceStorage])
	assert.Equal(t, &ssd, vct.StorageClassName)
}

func TestBuildCHKStorageClass(t *testing.T) {
	fast := "fast"
	engineOnFast := withEngine(newReplicatedTestInstance(""), storage("50Gi", &fast))

	// A new Keeper follows the engine's class.
	chk, _ := mustBuildCHK(t, engineOnFast)
	assert.Equal(t, &fast, chk.Spec.Templates.VolumeClaimTemplates[0].Spec.StorageClassName)

	// A deployed Keeper keeps its class, here the cluster default it was created
	// with before the keeper component existed.
	deployed, _ := mustBuildCHK(t, newReplicatedTestInstance(""))
	chk, _ = mustBuildCHK(t, engineOnFast, deployed)
	assert.Nil(t, chk.Spec.Templates.VolumeClaimTemplates[0].Spec.StorageClassName)
}

func TestValidateKeeper(t *testing.T) {
	keeperReplicas := func(n int32) func(*corev1alpha1.ComponentSpec) {
		return func(k *corev1alpha1.ComponentSpec) { k.Replicas = &n }
	}
	assert.NoError(t, validate(t, withKeeper(newReplicatedTestInstance(""), keeperReplicas(3))))
	assert.ErrorContains(t, validate(t, withKeeper(newReplicatedTestInstance(""), keeperReplicas(5))), "cannot be changed")
	assert.ErrorContains(t, validate(t, withKeeper(newTestInstance("db", "ns", ""), func(*corev1alpha1.ComponentSpec) {})),
		"only used by the replicated topology")
}

func TestValidateKeeperTransitions(t *testing.T) {
	base := newReplicatedTestInstance("")
	keeperStorage := func(size string, storageClass *string) func(*corev1alpha1.ComponentSpec) {
		return func(k *corev1alpha1.ComponentSpec) {
			k.Storage = &corev1alpha1.Storage{Size: resource.MustParse(size), StorageClass: storageClass}
		}
	}
	live, _ := mustBuildCHK(t, withKeeper(base, keeperStorage("20Gi", nil)))

	assert.NoError(t, validate(t, withKeeper(base, keeperStorage("20Gi", nil)), live))
	assert.NoError(t, validate(t, withKeeper(base, keeperStorage("30Gi", nil)), live))
	assert.ErrorContains(t, validate(t, base, live), "keeper storage cannot shrink from 20Gi to 10Gi")

	// An unset class keeps the deployed one, even when the engine uses another.
	fast := "fast"
	assert.NoError(t, validate(t, withEngine(withKeeper(base, keeperStorage("20Gi", nil)), storage("25Gi", &fast)), live))
	assert.ErrorContains(t, validate(t, withKeeper(base, keeperStorage("20Gi", &fast)), live),
		`keeper storageClass cannot change from the cluster default to "fast"`)

	newer := withKeeper(base, func(k *corev1alpha1.ComponentSpec) { k.Image = "clickhouse/clickhouse-keeper:25.8.1" })
	liveNewer, _ := mustBuildCHK(t, newer)
	assert.NoError(t, validate(t, newer, liveNewer))
	assert.ErrorContains(t, validate(t, base, liveNewer), "keeper cannot be downgraded from 25.8.1 to 25.3.5")
}
