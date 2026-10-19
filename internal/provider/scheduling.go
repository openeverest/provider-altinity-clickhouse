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
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// applySchedulingPolicy places a component's pods. The operator passes the pod
// template's scheduling fields through unchanged, merging only the affinity
// derived from podDistribution, which the provider does not set.
//
// When the policy carries no affinity, a preferred pod anti-affinity spreads
// the pods across nodes and zones; an explicit affinity, including an empty
// one, replaces that default.
func applySchedulingPolicy(spec *corev1.PodSpec, policy *commonv1alpha1.SchedulingPolicy, podLabels map[string]string) {
	spec.TopologySpreadConstraints = controller.TopologySpreadConstraints(policy, podLabels)
	spec.Affinity = defaultAffinity(podLabels)
	if policy == nil {
		return
	}
	spec.SchedulerName = policy.SchedulerName
	spec.NodeSelector = policy.NodeSelector
	spec.Tolerations = policy.Tolerations
	if policy.Affinity != nil {
		spec.Affinity = policy.Affinity
	}
}

// applyKeeperSchedulingPolicy places the Keeper pods. Keeper has no scheduling
// policy of its own, so it follows the engine onto the same nodes (scheduler,
// node selector, tolerations and node affinity) while always spreading its own
// pods: the engine's pod (anti-)affinity terms select engine pods and do not
// carry over.
func applyKeeperSchedulingPolicy(spec *corev1.PodSpec, enginePolicy *commonv1alpha1.SchedulingPolicy, podLabels map[string]string) {
	spec.Affinity = defaultAffinity(podLabels)
	if enginePolicy == nil {
		return
	}
	spec.SchedulerName = enginePolicy.SchedulerName
	spec.NodeSelector = enginePolicy.NodeSelector
	spec.Tolerations = enginePolicy.Tolerations
	if enginePolicy.Affinity != nil && enginePolicy.Affinity.NodeAffinity != nil {
		spec.Affinity.NodeAffinity = enginePolicy.Affinity.NodeAffinity.DeepCopy()
	}
}

// defaultAffinity prefers placing pods matching podLabels on distinct nodes
// and, where nodes carry zone labels, in distinct zones. It is preferred rather
// than required so instances stay schedulable on clusters with fewer nodes
// than replicas.
func defaultAffinity(podLabels map[string]string) *corev1.Affinity {
	term := func(weight int32, topologyKey string) corev1.WeightedPodAffinityTerm {
		return corev1.WeightedPodAffinityTerm{
			Weight: weight,
			PodAffinityTerm: corev1.PodAffinityTerm{
				LabelSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(podLabels)},
				TopologyKey:   topologyKey,
			},
		}
	}
	return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
			term(common.AntiAffinityHostnameWeight, corev1.LabelHostname),
			term(common.AntiAffinityZoneWeight, corev1.LabelTopologyZone),
		},
	}}
}
