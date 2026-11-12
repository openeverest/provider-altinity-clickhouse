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

// Package replicated contains the parameter types of the replicated topology.
//
// The ReplicatedTopologyParameters struct is referenced by topology.yaml via
// parametersSchema and is converted to an OpenAPI schema during generation.
//
// +k8s:openapi-gen=true
package replicated

// ReplicatedTopologyParameters defines the parameters of the replicated
// topology.
type ReplicatedTopologyParameters struct {
	// NumShards is the number of shards. Each shard holds part of the data
	// and runs the engine's replicas. Shards can be added, not removed, and
	// existing data is not moved onto new shards. Defaults to 1.
	// +kubebuilder:validation:Minimum=1
	// +default=1
	// +optional
	NumShards int32 `json:"numShards,omitempty"`
}
