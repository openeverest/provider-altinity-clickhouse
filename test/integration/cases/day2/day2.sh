#!/bin/sh
# Day-2 operations on a running replicated Instance: a configuration change,
# losing a ClickHouse pod, losing a Keeper pod, and growing the storage.
set -eu
NAME=itest-ch-day2
. ../../lib/clickhouse.sh

rows() { q "$1" "SELECT count() FROM t"; }
keepers_ready() {
  kubectl get pods -n "$NS" -l "clickhouse-keeper.altinity.com/chk=$NAME-keeper" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{" "}{end}'
}
setting() { q "$1" "SELECT value FROM system.server_settings WHERE name = 'max_concurrent_queries'"; }

wait_until "the cluster lists 2 replicas" equals 2 replicas_in_cluster
q 0 "CREATE TABLE t ON CLUSTER '{cluster}' (id UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/t', '{replica}') ORDER BY id"
q 0 "INSERT INTO t SELECT number FROM numbers(1000)"

echo "== configuration change"
# The operator restarts ClickHouse for setting changes, one replica at a time.
patch_instance '{"spec":{"components":{"engine":{"parameters":{"configuration":"max_concurrent_queries: 150"}}}}}'
wait_ready
[ "$(setting 0)" = 150 ] || fail "replica 0 runs max_concurrent_queries=$(setting 0)"
[ "$(setting 1)" = 150 ] || fail "replica 1 runs max_concurrent_queries=$(setting 1)"
echo "ok: both replicas run the new setting"

echo "== losing a ClickHouse pod"
kubectl delete pod -n "$NS" "$(pod 1)" --wait=false
q 0 "INSERT INTO t SELECT 1000 + number FROM numbers(100)"
wait_until "replica 1 is back and caught up" equals 1100 rows 1
wait_until "Instance Ready" ready

echo "== losing a Keeper pod"
kubectl delete pod -n "$NS" "chk-$NAME-keeper-keeper-0-0-0" --wait=false
# Two of three Keeper nodes keep the quorum. A retried insert of the same
# block is deduplicated, so retrying across a session move is safe.
wait_until "inserts succeed while a Keeper node is down" q 0 "INSERT INTO t SELECT 1100 + number FROM numbers(100)"
wait_until "all Keeper nodes ready again" equals "true true true " keepers_ready
wait_until "replica 1 caught up" equals 1200 rows 1

echo "== storage growth"
class=$(kubectl get pvc -n "$NS" "data-$(pod 0)" -o jsonpath='{.spec.storageClassName}')
patch_instance '{"spec":{"components":{"engine":{"storage":{"size":"20Gi"}}}}}'
if [ "$(kubectl get storageclass "$class" -o jsonpath='{.allowVolumeExpansion}')" = true ]; then
  capacities() {
    kubectl get pvc -n "$NS" -l "clickhouse.altinity.com/chi=$NAME" -o jsonpath='{range .items[*]}{.status.capacity.storage}{" "}{end}'
  }
  wait_ready
  wait_until "both volumes grew to 20Gi" equals "20Gi 20Gi " capacities
else
  # The CI cluster's local-path class cannot expand volumes: the provider
  # rejects the change rather than leave the operator's resize stuck.
  wait_until "the $class StorageClass cannot expand: the change is rejected" equals Failed phase
  requested=$(kubectl get clickhouseinstallations.clickhouse.altinity.com "$NAME" -n "$NS" \
    -o jsonpath='{.spec.templates.volumeClaimTemplates[?(@.name=="data")].spec.resources.requests.storage}')
  [ "$requested" = 10Gi ] || fail "a rejected change must not reach the CHI, which requests $requested"
  patch_instance '{"spec":{"components":{"engine":{"storage":{"size":"10Gi"}}}}}'
  wait_until "reverting the size recovers the Instance" ready
fi
