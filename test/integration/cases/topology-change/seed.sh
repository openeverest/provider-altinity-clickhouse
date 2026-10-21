#!/bin/sh
set -eu
NAME=itest-ch-topology
. ../../lib/clickhouse.sh
q 0 "CREATE TABLE m (id UInt64) ENGINE = MergeTree ORDER BY id"
q 0 "INSERT INTO m SELECT number FROM numbers(500)"
