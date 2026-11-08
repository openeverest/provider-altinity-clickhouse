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

	chiv1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-altinity-clickhouse/definition/topologies/sharded"
	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// isSharded reports whether the Instance asks for the sharded topology.
func isSharded(c *controller.Context) bool {
	return c.Instance().GetTopologyType() == common.TopologySharded
}

// usesKeeperType reports whether the topology runs a ClickHouse Keeper quorum.
func usesKeeperType(topology string) bool {
	switch topology {
	case common.TopologyReplicated, common.TopologySharded:
		return true
	}
	return false
}

// usesKeeper reports whether the Instance's topology runs a ClickHouse Keeper
// quorum: replicated and sharded do, standalone does not.
func usesKeeper(c *controller.Context) bool {
	return usesKeeperType(c.Instance().GetTopologyType())
}

// shardsCountFromInstance returns the number of shards for instance.
func shardsCountFromInstance(instance *corev1alpha1.Instance) int {
	if instance == nil || instance.GetTopologyType() != common.TopologySharded {
		return 1
	}
	if topology := instance.Spec.Topology; topology != nil && topology.Parameters != nil {
		var params sharded.ShardedTopologyParameters
		if err := json.Unmarshal(topology.Parameters.Raw, &params); err == nil && params.Shards > 0 {
			return int(params.Shards)
		}
	}
	return common.DefaultShardsCount
}

// shardsCount returns the number of shards: the sharded topology's
// parameter (default common.DefaultShardsCount), and 1 for every other
// topology.
func shardsCount(c *controller.Context) int {
	return shardsCountFromInstance(c.Instance())
}

// liveShardsCount returns the shard count of the deployed cluster, 1 when the
// layout does not say.
func liveShardsCount(chi *chiv1.ClickHouseInstallation) int {
	if chi.Spec.Configuration == nil {
		return 1
	}
	for _, cluster := range chi.Spec.Configuration.Clusters {
		if cluster == nil || cluster.Name != common.CHIClusterName || cluster.Layout == nil {
			continue
		}
		if cluster.Layout.ShardsCount > 0 {
			return cluster.Layout.ShardsCount
		}
	}
	return 1
}
