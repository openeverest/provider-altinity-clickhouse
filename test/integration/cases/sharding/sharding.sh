#!/bin/sh
# Spreads rows over the shards through a Distributed table, adds a shard
# (the operator creates the tables on it; existing rows stay where they
# are, new ones reach it) and checks the rows through any shard.
set -eu
NAME=itest-ch-sharding
. ../../lib/clickhouse.sh

local_rows() { qs "$1" 0 "SELECT count() FROM default.events_local"; }
total() { qs "$1" 0 "SELECT count() FROM default.events"; }
tables() { qs "$1" 0 "SELECT arrayStringConcat(arraySort(groupArray(concat(name, ':', engine))), ',') FROM system.tables WHERE database = 'default'"; }

wait_until "the cluster lists 2 shards" equals 2 replicas_in_cluster
q 0 "CREATE TABLE default.events_local ON CLUSTER '{cluster}' (id UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/events_local', '{replica}') ORDER BY id"
q 0 "CREATE TABLE default.events ON CLUSTER '{cluster}' AS default.events_local ENGINE = Distributed('{cluster}', default, events_local, cityHash64(id))"
q 0 "INSERT INTO default.events SELECT number FROM numbers(10000) SETTINGS insert_distributed_sync = 1"

[ "$(total 1)" = 10000 ] || fail "the Distributed table returns $(total 1) rows, expected 10000"
s0=$(local_rows 0) s1=$(local_rows 1)
[ "$s0" -gt 0 ] && [ "$s1" -gt 0 ] && [ $((s0 + s1)) = 10000 ] || fail "rows per shard: $s0 + $s1"
echo "ok: 10000 rows spread over 2 shards ($s0 + $s1)"

echo "== add a shard"
patch_instance '{"spec":{"topology":{"parameters":{"numShards":3}}}}'
wait_ready
wait_until "the cluster lists 3 shards" equals 3 replicas_in_cluster
want="events:Distributed,events_local:ReplicatedMergeTree"
[ "$(tables 2)" = "$want" ] || fail "the new shard has tables $(tables 2), expected $want"
[ "$(local_rows 2)" = 0 ] || fail "existing rows are not moved onto a new shard, it has $(local_rows 2)"
[ "$(total 2)" = 10000 ] || fail "the new shard reads $(total 2) rows through the Distributed table"
echo "ok: the new shard got the tables and reads every row"

q 0 "INSERT INTO default.events SELECT 10000 + number FROM numbers(3000) SETTINGS insert_distributed_sync = 1"
[ "$(local_rows 2)" -gt 0 ] || fail "new rows should reach the new shard"
[ "$(total 0)" = 13000 ] || fail "the Distributed table returns $(total 0) rows, expected 13000"
echo "ok: new rows reach the new shard ($(local_rows 2) of 3000)"
