#!/bin/sh
# After switching to replicated: the existing MergeTree table keeps its rows
# on the first replica (tables are not converted), its schema reaches the new
# replica, and copying the data into a replicated table replicates it.
set -eu
NAME=itest-ch-topology
. ../../lib/clickhouse.sh

wait_until "the cluster lists 2 replicas" equals 2 replicas_in_cluster
[ "$(q 0 'SELECT count() FROM m')" = 500 ] || fail "the existing table lost rows"
wait_until "the new replica has the table's schema" \
  equals MergeTree q 1 "SELECT engine FROM system.tables WHERE database = 'default' AND name = 'm'"
q 0 "CREATE TABLE r ON CLUSTER '{cluster}' (id UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/r', '{replica}') ORDER BY id"
q 0 "INSERT INTO r SELECT id FROM m"
wait_until "the copied rows replicate" equals 500 q 1 "SELECT count() FROM r"
