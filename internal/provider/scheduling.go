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
	corev1 "k8s.io/api/core/v1"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// applySchedulingPolicy places a component's pods. The operator passes the pod
// template's scheduling fields through unchanged, merging only the affinity
// derived from podDistribution, which the provider does not set.
func applySchedulingPolicy(spec *corev1.PodSpec, policy *commonv1alpha1.SchedulingPolicy, podLabels map[string]string) {
	spec.TopologySpreadConstraints = controller.TopologySpreadConstraints(policy, podLabels)
	if policy == nil {
		return
	}
	spec.SchedulerName = policy.SchedulerName
	spec.NodeSelector = policy.NodeSelector
	spec.Affinity = policy.Affinity
	spec.Tolerations = policy.Tolerations
}
