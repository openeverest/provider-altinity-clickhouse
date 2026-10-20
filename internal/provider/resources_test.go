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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

func list(cpu, memory string) corev1.ResourceList {
	l := corev1.ResourceList{}
	if cpu != "" {
		l[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		l[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	return l
}

func TestResolveResources(t *testing.T) {
	tests := []struct {
		name                      string
		declared                  *corev1.ResourceRequirements
		limitCPU, limitMemory     string
		requestCPU, requestMemory string
	}{
		{name: "defaults", limitCPU: "1", limitMemory: "4Gi", requestCPU: "1", requestMemory: "4Gi"},
		{name: "limits only: requests follow", declared: &corev1.ResourceRequirements{Limits: list("2", "8Gi")},
			limitCPU: "2", limitMemory: "8Gi", requestCPU: "2", requestMemory: "8Gi"},
		{name: "requests only: default limits", declared: &corev1.ResourceRequirements{Requests: list("500m", "2G")},
			limitCPU: "1", limitMemory: "4Gi", requestCPU: "500m", requestMemory: "2G"},
		{name: "requests above the default limit raise it", declared: &corev1.ResourceRequirements{Requests: list("2", "")},
			limitCPU: "2", limitMemory: "4Gi", requestCPU: "2", requestMemory: "4Gi"},
		{name: "both", declared: &corev1.ResourceRequirements{Requests: list("1", "2Gi"), Limits: list("2", "4Gi")},
			limitCPU: "2", limitMemory: "4Gi", requestCPU: "1", requestMemory: "2Gi"},
		{name: "a zero value counts as unset", declared: &corev1.ResourceRequirements{Limits: list("0", "")},
			limitCPU: "1", limitMemory: "4Gi", requestCPU: "1", requestMemory: "4Gi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveResources(corev1alpha1.ComponentSpec{Resources: tt.declared})
			assert.Equal(t, tt.limitCPU, got.Limits.Cpu().String())
			assert.Equal(t, tt.limitMemory, got.Limits.Memory().String())
			assert.Equal(t, tt.requestCPU, got.Requests.Cpu().String())
			assert.Equal(t, tt.requestMemory, got.Requests.Memory().String())
		})
	}
}

func TestResolveKeeperResourcesDefaults(t *testing.T) {
	got := resolveKeeperResources(corev1alpha1.ComponentSpec{Resources: &corev1.ResourceRequirements{Requests: list("250m", "")}})
	assert.Equal(t, common.KeeperDefaultCPU, got.Limits.Cpu().String())
	assert.Equal(t, "250m", got.Requests.Cpu().String())
	assert.Equal(t, common.KeeperDefaultMemory, got.Requests.Memory().String())
}

// The README example used to set requests, which were silently ignored.
func TestBuildCHIHonoursRequests(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	engine := instance.Spec.Components[common.ComponentEngine]
	engine.Resources = &corev1.ResourceRequirements{Requests: list("500m", "2G")}
	instance.Spec.Components[common.ComponentEngine] = engine

	chi, err := buildCHI(newTestContext(t, instance), 1)
	require.NoError(t, err)
	container := chi.Spec.Templates.PodTemplates[0].Spec.Containers[0]
	assert.Equal(t, "500m", container.Resources.Requests.Cpu().String())
	assert.Equal(t, "2G", container.Resources.Requests.Memory().String())
	assert.Equal(t, "1", container.Resources.Limits.Cpu().String())
	assert.Equal(t, "4Gi", container.Resources.Limits.Memory().String())
}

func TestValidateRejectsRequestAboveLimit(t *testing.T) {
	instance := newTestInstance("db", "ns", "")
	engine := instance.Spec.Components[common.ComponentEngine]
	engine.Resources = &corev1.ResourceRequirements{Requests: list("", "8Gi"), Limits: list("", "4Gi")}
	instance.Spec.Components[common.ComponentEngine] = engine
	err := New().Validate(newTestContext(t, instance))
	assert.ErrorContains(t, err, "engine memory request 8Gi exceeds its limit 4Gi")

	keeper := replicatedWithKeeper(&corev1.ResourceRequirements{Requests: list("2", ""), Limits: list("1", "")})
	err = New().Validate(newTestContext(t, keeper))
	assert.ErrorContains(t, err, "keeper cpu request 2 exceeds its limit 1")
}

func replicatedWithKeeper(resources *corev1.ResourceRequirements) *corev1alpha1.Instance {
	instance := newTestInstance("db", "ns", "")
	instance.Spec.Topology = &corev1alpha1.TopologySpec{Type: common.TopologyReplicated}
	replicas := int32(2)
	engine := instance.Spec.Components[common.ComponentEngine]
	engine.Replicas = &replicas
	instance.Spec.Components[common.ComponentEngine] = engine
	instance.Spec.Components[common.ComponentKeeper] = corev1alpha1.ComponentSpec{Type: "clickhouse-keeper", Resources: resources}
	return instance
}
