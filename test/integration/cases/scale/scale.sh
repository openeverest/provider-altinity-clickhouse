#!/bin/sh
# Scales a replicated Instance up, down and back up under the default Retain
# policy: a new replica gets the tables and their rows, a removed replica
# keeps its volume and Keeper metadata, and it rejoins and catches up.
set -eu
NAME=itest-ch-scale
. ../../lib/clickhouse.sh

active() { q 0 "SELECT concat(toString(total_replicas), '/', toString(active_replicas)) FROM system.replicas WHERE table = 't'"; }
pods() { kubectl get pods -n "$NS" -l "clickhouse.altinity.com/chi=$NAME" --no-headers | wc -l | tr -d ' '; }

wait_until "the cluster lists 2 replicas" equals 2 replicas_in_cluster
q 0 "CREATE TABLE t ON CLUSTER '{cluster}' (id UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/t', '{replica}') ORDER BY id"
q 0 "INSERT INTO t SELECT number FROM numbers(1000)"

echo "== scale up to 3"
patch_instance '{"spec":{"components":{"engine":{"replicas":3}}}}'
wait_ready
wait_until "the new replica has the table and its rows" equals 1000 q 2 "SELECT count() FROM t"
wait_until "the cluster lists 3 replicas" equals 3 replicas_in_cluster

echo "== scale down to 2"
patch_instance '{"spec":{"components":{"engine":{"replicas":2}}}}'
wait_ready
wait_until "2 ClickHouse pods" equals 2 pods
kubectl get pvc -n "$NS" "data-$(pod 2)" >/dev/null || fail "the removed replica's volume should be retained"
[ "$(active)" = 3/2 ] || fail "the removed replica should stay registered in Keeper, got $(active) total/active"
q 0 "INSERT INTO t SELECT 1000 + number FROM numbers(100)"
echo "ok: volume and Keeper metadata retained; writes continue"

echo "== scale back up to 3"
patch_instance '{"spec":{"components":{"engine":{"replicas":3}}}}'
wait_ready
wait_until "the returning replica caught up" equals 1100 q 2 "SELECT count() FROM t"
wait_until "3 of 3 replicas active" equals 3/3 active
