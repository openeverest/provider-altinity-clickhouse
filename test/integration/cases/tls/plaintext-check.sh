#!/bin/sh
# After TLS is turned off: the connection details point at plain HTTP without
# a CA, ClickHouse reaches Keeper on its plaintext port, and a distributed
# query and replication work over the plaintext ports.
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).
set -eu
NAME=itest-ch-tls
. ../../lib/clickhouse.sh

conn() { kubectl get secret "$NAME-conn" -n "$NS" -o jsonpath="{.data.$1}" | base64 -d; }
wait_until "connection details on plain HTTP" equals 8123 conn port
[ -z "$(kubectl get secret "$NAME-conn" -n "$NS" -o jsonpath='{.data.ca\.crt}')" ] || fail "the connection Secret still carries a CA"
case "$(conn uri)" in http://*) ;; *) fail "uri is $(conn uri)" ;; esac

for r in 0 1; do
  port=$(q "$r" "SELECT port FROM system.zookeeper_connection")
  [ "$port" = 2181 ] || fail "replica $r reaches Keeper on $port"
  for setting in https_port tcp_port_secure; do
    [ "$(q "$r" "SELECT count() FROM system.server_settings WHERE name = '$setting' AND changed")" = 0 ] ||
      fail "replica $r still sets $setting"
  done
done

wait_until "both replicas registered" equals 2 replicas_in_cluster
q 0 "CREATE TABLE IF NOT EXISTS default.plain_probe ON CLUSTER '{cluster}' (id UInt64)
     ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/plain_probe', '{replica}') ORDER BY id"
q 0 "INSERT INTO default.plain_probe VALUES (1)"
wait_until "the row replicated over plain HTTP" equals 1 q 1 "SELECT count() FROM default.plain_probe"
hosts=$(q 0 "SELECT count(DISTINCT hostName()) FROM clusterAllReplicas('clickhouse', system.one)")
[ "$hosts" = 2 ] || fail "a distributed query reached $hosts replicas"
echo "plaintext OK: Keeper on 2181, replication and distributed queries work"
