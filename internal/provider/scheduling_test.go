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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

func TestBuildCHIWithoutSchedulingPolicyLeavesPlacementToScheduler(t *testing.T) {
	c := newTestContext(t, newTestInstance("db", "ns", ""))

	chi, err := buildCHI(c, 2)
	require.NoError(t, err)

	spec := chi.Spec.Templates.PodTemplates[0].Spec
	assert.Nil(t, spec.Affinity)
	assert.Nil(t, spec.TopologySpreadConstraints)
	assert.Nil(t, spec.NodeSelector)
	assert.Nil(t, spec.Tolerations)
	assert.Empty(t, spec.SchedulerName)
}

func TestBuildCHIAppliesSchedulingPolicy(t *testing.T) {
	affinity := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: corev1.LabelHostname}},
	}}
	tolerations := []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "db", Effect: corev1.TaintEffectNoSchedule}}
	ownSelector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}}
	spread := []corev1.TopologySpreadConstraint{
		{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.ScheduleAnyway},
		{MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.DoNotSchedule, LabelSelector: ownSelector},
	}

	instance := newTestInstance("db", "ns", "")
	engine := instance.Spec.Components[common.ComponentEngine]
	engine.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{
		SchedulerName:             "custom-scheduler",
		NodeSelector:              map[string]string{"node-role": "db"},
		Affinity:                  affinity,
		Tolerations:               tolerations,
		TopologySpreadConstraints: &spread,
	}
	instance.Spec.Components[common.ComponentEngine] = engine
	c := newTestContext(t, instance)

	chi, err := buildCHI(c, 2)
	require.NoError(t, err)

	template := chi.Spec.Templates.PodTemplates[0]
	assert.Equal(t, "custom-scheduler", template.Spec.SchedulerName)
	assert.Equal(t, map[string]string{"node-role": "db"}, template.Spec.NodeSelector)
	assert.Equal(t, affinity, template.Spec.Affinity)
	assert.Equal(t, tolerations, template.Spec.Tolerations)

	require.Len(t, template.Spec.TopologySpreadConstraints, 2)
	// A constraint without its own selector counts the engine pods.
	assert.Equal(t, template.ObjectMeta.Labels, template.Spec.TopologySpreadConstraints[0].LabelSelector.MatchLabels)
	assert.Equal(t, ownSelector, template.Spec.TopologySpreadConstraints[1].LabelSelector)
}
