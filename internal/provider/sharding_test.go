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
	"testing"

	"github.com/google/cel-go/cel"
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

// scalingRule returns a CEL rule of the replicated Scaling section.
func scalingRule(t *testing.T, field string, edit bool) string {
	t.Helper()
	var ui struct {
		Replicated struct {
			Sections map[string]struct {
				Components map[string]struct {
					Validation struct {
						CelExpressions []struct {
							CelExpr string `json:"celExpr"`
						} `json:"celExpressions"`
						Modes struct {
							Edit struct {
								CelExpressions []struct {
									CelExpr string `json:"celExpr"`
								} `json:"celExpressions"`
							} `json:"edit"`
						} `json:"modes"`
					} `json:"validation"`
				} `json:"components"`
			} `json:"sections"`
		} `json:"replicated"`
	}
	require.NoError(t, json.Unmarshal(testProvider(t).Spec.UISchema.Raw, &ui))
	v := ui.Replicated.Sections["scaling"].Components[field].Validation
	rules := v.CelExpressions
	if edit {
		rules = v.Modes.Edit.CelExpressions
	}
	require.Len(t, rules, 1)
	return rules[0].CelExpr
}

func evalUIRule(t *testing.T, expr string, vars map[string]any) (bool, error) {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("spec", cel.DynType), cel.Variable("original", cel.DynType))
	require.NoError(t, err)
	ast, iss := env.Compile(expr)
	require.NoError(t, iss.Err())
	prg, err := env.Program(ast)
	require.NoError(t, err)
	out, _, err := prg.Eval(vars)
	if err != nil {
		return false, err
	}
	return out.Value().(bool), nil
}

func topologySpec(parameters map[string]any) map[string]any {
	topology := map[string]any{"type": "replicated"}
	if parameters != nil {
		topology["parameters"] = parameters
	}
	return map[string]any{"topology": topology}
}

// On OpenEverest 2.0.0-dev.4 `original` is the stored Instance: one created
// before sharding has no numShards, and a rule that errors on it would keep
// every edit dialog of that instance from saving.
func TestUIShardsRuleOnlyRejectsRemovingShards(t *testing.T) {
	expr := scalingRule(t, "shards", true)
	for _, tc := range []struct {
		name     string
		form     any
		original map[string]any
		want     bool
	}{
		{"created before sharding", 1, nil, true},
		{"created before sharding, adding shards", 3, nil, true},
		{"empty parameters", 2, map[string]any{}, true},
		{"unchanged", 2, map[string]any{"numShards": 2}, true},
		{"raw string from another dialog", "2", map[string]any{"numShards": 2}, true},
		{"adding", 3, map[string]any{"numShards": 2}, true},
		{"removing", 1, map[string]any{"numShards": 2}, false},
		{"removing, as a string", "1", map[string]any{"numShards": 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalUIRule(t, expr, map[string]any{
				"spec":     topologySpec(map[string]any{"numShards": tc.form}),
				"original": map[string]any{"spec": topologySpec(tc.original)},
			})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUIReplicasRuleNeedsTwoForASingleShard(t *testing.T) {
	expr := scalingRule(t, "replicas", false)
	for _, tc := range []struct {
		replicas, shards any
		want             bool
	}{
		{1, 1, false},
		{2, 1, true},
		{1, 2, true},
		{"1", "2", true},
		{"1", "1", false},
	} {
		spec := topologySpec(map[string]any{"numShards": tc.shards})
		spec["components"] = map[string]any{"engine": map[string]any{"replicas": tc.replicas}}
		got, err := evalUIRule(t, expr, map[string]any{"spec": spec, "original": map[string]any{}})
		require.NoError(t, err, "%v replicas, %v shards", tc.replicas, tc.shards)
		assert.Equal(t, tc.want, got, "%v replicas, %v shards", tc.replicas, tc.shards)
	}
}
