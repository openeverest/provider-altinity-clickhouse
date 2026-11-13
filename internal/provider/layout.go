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

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/topologies/replicated"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// clusterLayout is the shape of the ClickHouse cluster: shards, replicas in
// each, and whether ClickHouse is coordinated by Keeper.
type clusterLayout struct {
	shards, replicas int
	keeper           bool
}

// hosts is the number of ClickHouse pods.
func (l clusterLayout) hosts() int { return l.shards * l.replicas }

// layoutOf returns the layout the Instance asks for: standalone runs one
// host without Keeper; replicated runs numShards shards of replicas each,
// coordinated by Keeper.
func layoutOf(instance *corev1alpha1.Instance) clusterLayout {
	if instance.GetTopologyType() != common.TopologyReplicated {
		return clusterLayout{shards: 1, replicas: 1}
	}
	return clusterLayout{shards: numShards(instance), replicas: replicasPerShard(instance), keeper: true}
}

// layout returns the layout of the Instance being reconciled.
func layout(c *controller.Context) clusterLayout { return layoutOf(c.Instance()) }

// numShards returns the replicated topology's number of shards, 1 unless set.
func numShards(instance *corev1alpha1.Instance) int {
	params := instance.GetTopologyParameters()
	if params == nil || len(params.Raw) == 0 {
		return 1
	}
	var p replicated.ReplicatedTopologyParameters
	// The runtime has checked the parameters against their schema.
	if err := json.Unmarshal(params.Raw, &p); err != nil || p.NumShards < 1 {
		return 1
	}
	return int(p.NumShards)
}

// replicasPerShard returns the engine's replicas in each shard. A single
// shard needs at least 2 for high availability; with more shards, 1 is
// accepted (scale-out without replication). Anything less than that falls
// back to the default, which validation rejects anyway.
func replicasPerShard(instance *corev1alpha1.Instance) int {
	engine := instance.Spec.Components[common.ComponentEngine]
	if engine.Replicas != nil {
		if r := int(*engine.Replicas); r >= minReplicasPerShard(numShards(instance)) {
			return r
		}
	}
	return common.DefaultReplicasCount
}

// minReplicasPerShard is the fewest replicas a shard may run.
func minReplicasPerShard(shards int) int {
	if shards >= 2 {
		return 1
	}
	return 2
}
