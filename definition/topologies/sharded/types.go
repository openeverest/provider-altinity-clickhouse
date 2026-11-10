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

// Package sharded holds the sharded topology's parameter types.
//
// +k8s:openapi-gen=true
package sharded

// ShardedTopologyParameters configures the sharded topology
// (Instance.spec.topology.parameters).
type ShardedTopologyParameters struct {
	// Shards is the number of shards. Each shard runs
	// spec.components.engine.replicas replicas. Shards can be added but not
	// removed, and existing data is not rebalanced onto new shards.
	// +kubebuilder:validation:Minimum=2
	Shards int32 `json:"shards,omitempty"`
}
