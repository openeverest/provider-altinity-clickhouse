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
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-altinity-clickhouse/internal/common"
)

// shardedInstance is a replicated Instance with n shards of r replicas.
func shardedInstance(n int, r int32) *corev1alpha1.Instance {
	instance := newReplicatedTestInstance("")
	shards(n)(instance)
	return withEngine(instance, replicas(r))
}

func TestLayoutOf(t *testing.T) {
	tests := []struct {
		name     string
		instance *corev1alpha1.Instance
		want     clusterLayout
	}{
		{name: "standalone", instance: newTestInstance("db", "ns", ""), want: clusterLayout{shards: 1, replicas: 1}},
		{name: "replicated without parameters", instance: newReplicatedTestInstance(""),
			want: clusterLayout{shards: 1, replicas: common.DefaultReplicasCount, keeper: true}},
		{name: "three shards of two", instance: shardedInstance(3, 2), want: clusterLayout{shards: 3, replicas: 2, keeper: true}},
		{name: "one replica per shard", instance: shardedInstance(2, 1), want: clusterLayout{shards: 2, replicas: 1, keeper: true}},
		// Rejected by Validate; Sync never sees it.
		{name: "one replica in a single shard", instance: shardedInstance(1, 1),
			want: clusterLayout{shards: 1, replicas: common.DefaultReplicasCount, keeper: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, layoutOf(tt.instance))
		})
	}
}

func TestBuildCHISharded(t *testing.T) {
	instance := shardedInstance(3, 1)
	c := newTestContext(t, instance)
	chi, err := buildCHI(c, layout(c))
	require.NoError(t, err)
	cluster := chi.Spec.Configuration.Clusters[0]
	assert.Equal(t, 3, cluster.Layout.ShardsCount)
	assert.Equal(t, 1, cluster.Layout.ReplicasCount)
	require.NotNil(t, chi.Spec.Configuration.Zookeeper, "ON CLUSTER DDL and replicated tables need Keeper, also with one replica per shard")
	assert.Equal(t, "db-keeper", chi.Spec.Configuration.Zookeeper.Keeper.Name)
}

// Existing replicated Instances carry no numShards: they render exactly as
// before sharding existed, so a provider upgrade restarts nothing.
func TestShardsParameterKeepsExistingRendering(t *testing.T) {
	without := newReplicatedTestInstance("")
	c := newTestContext(t, without)
	before, err := buildCHI(c, layout(c))
	require.NoError(t, err)
	beforeHash, err := specHash(before)
	require.NoError(t, err)

	explicit := shardedInstance(1, common.DefaultReplicasCount)
	c = newTestContext(t, explicit)
	after, err := buildCHI(c, layout(c))
	require.NoError(t, err)
	afterHash, err := specHash(after)
	require.NoError(t, err)
	assert.Equal(t, beforeHash, afterHash)
}

func TestServerCertificateCoversEveryShard(t *testing.T) {
	names := serverDNSNames("db", "ns", clusterLayout{shards: 2, replicas: 2, keeper: true})
	for _, name := range []string{
		"chi-db-clickhouse-0-0.ns.svc.cluster.local",
		"chi-db-clickhouse-0-1.ns.svc.cluster.local",
		"chi-db-clickhouse-1-0.ns.svc.cluster.local",
		"chi-db-clickhouse-1-1.ns.svc.cluster.local",
		"*.chi-db-clickhouse-1-1.ns.svc.cluster.local",
	} {
		assert.Contains(t, names, name)
	}
}

// The running cluster is the last line of defence against removing shards,
// e.g. when the change was stored without admission.
func TestValidateReferencesRejectsRemovingShards(t *testing.T) {
	live := deployedCHIWithLayout(t, shardedInstance(3, 1))

	errs, err := New().ValidateReferences(newTestContext(t, shardedInstance(2, 1), live))
	require.NoError(t, err)
	require.Len(t, errs, 1)
	assert.Equal(t, "spec.topology.parameters.numShards", errs[0].Field)
	assert.Contains(t, errs[0].Detail, "cannot shrink from 3 to 2 shards")

	errs, err = New().ValidateReferences(newTestContext(t, shardedInstance(4, 1), live))
	require.NoError(t, err)
	assert.Empty(t, errs, "adding shards")
}

func deployedCHIWithLayout(t *testing.T, instance *corev1alpha1.Instance) client.Object {
	t.Helper()
	c := newTestContext(t, instance.DeepCopy())
	chi, err := buildCHI(c, layout(c))
	require.NoError(t, err)
	return chi
}

// Adding shards is the user's change: it applies right away, without waiting
// for maintenance approval.
func TestRolloutAppliesAddedShards(t *testing.T) {
	instance := shardedInstance(2, 1)
	_, first := syncReplicated(t, instance, completedCHK(), completedCHI())
	state := persisted(t, first, rolloutStateName("db"))

	kinds, c := syncReplicated(t, shardedInstance(3, 1), state, completedCHK(), completedCHI())
	assert.Contains(t, kinds, "ClickHouseInstallation")
	assert.Empty(t, c.GetPendingMaintenance())
}
