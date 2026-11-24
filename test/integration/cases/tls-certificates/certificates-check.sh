#!/bin/sh
# Bring your own CA: clients trusting only the company CA verify both
# Instances, by the in-cluster name and by the external one, and the
# replicas and Keeper of the Instance with its own certificate Secret work
# over TLS with it.
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).
set -eu
. ../../lib/clickhouse.sh

CA_FILE=/tmp/itest-company-ca.crt
company_ca=$(kubectl get secret itest-ch-company-ca -n cert-manager -o jsonpath='{.data.tls\.crt}')

# verified <pod> <host:port> <name>: the certificate served at host:port
# verifies against the company CA for name.
verified() {
  kubectl exec -n "$NS" "$1" -c clickhouse -- sh -c \
    "echo | openssl s_client -connect $2 -CAfile $CA_FILE -verify_return_error -verify_hostname $3 2>&1" |
    grep -q "Verify return code: 0 (ok)"
}

for NAME in itest-ch-issuer itest-ch-own; do
  [ "$(kubectl get secret "$NAME-conn" -n "$NS" -o jsonpath='{.data.ca\.crt}')" = "$company_ca" ] ||
    fail "$NAME: ca.crt of the connection Secret is not the company CA"
  echo "$company_ca" | base64 -d | kubectl exec -i -n "$NS" "$(pod 0)" -c clickhouse -- sh -c "cat > $CA_FILE"
  for port in 8443 9440; do
    verified "$(pod 0)" "clickhouse-$NAME.$NS.svc.cluster.local:$port" "clickhouse-$NAME.$NS.svc.cluster.local" ||
      fail "$NAME: port $port does not verify for the Service name"
    verified "$(pod 0)" "clickhouse-$NAME.$NS.svc.cluster.local:$port" "$NAME.company.com" ||
      fail "$NAME: port $port does not verify for $NAME.company.com"
  done
  if verified "$(pod 0)" "clickhouse-$NAME.$NS.svc.cluster.local:8443" "other.company.com"; then
    fail "$NAME: the certificate verifies for a name it should not carry"
  fi
  echo "ok: $NAME verifies against the company CA, in-cluster and as $NAME.company.com"
done

# The replicas and Keeper of itest-ch-own verify each other with the
# certificate of the user's Secret.
NAME=itest-ch-own
wait_until "both replicas registered" equals 2 replicas_in_cluster
ports=$(q 0 "SELECT groupUniqArray(port) FROM system.clusters WHERE cluster = 'clickhouse'")
[ "$ports" = "[9440]" ] || fail "remote_servers should use the secure port, got $ports"
hosts=$(q 0 "SELECT count(DISTINCT hostName()) FROM clusterAllReplicas('clickhouse', system.one)")
[ "$hosts" = 2 ] || fail "a distributed query over TLS reached $hosts replicas"
zk=$(q 0 "SELECT concat(host, ':', toString(port)) FROM system.zookeeper_connection")
case "$zk" in
  secure://*-client.*:2281) ;;
  *) fail "ClickHouse should reach Keeper over TLS, got $zk" ;;
esac
q 0 "CREATE TABLE IF NOT EXISTS default.own_tls ON CLUSTER '{cluster}' (id UInt64)
     ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/own_tls', '{replica}') ORDER BY id" >/dev/null
q 0 "INSERT INTO default.own_tls SELECT number FROM numbers(100)"
wait_until "the rows replicated over HTTPS" equals 100 q 1 "SELECT count() FROM default.own_tls"
echo "ok: distributed queries, Keeper and replication work with the user's certificate"

# The certificate source cannot change.
if out=$(kubectl patch instances.core.openeverest.io "$NAME" -n "$NS" --type=merge \
  -p '{"spec":{"parameters":{"tls":{"certificateSecretName":"itest-ch-external-only-tls"}}}}' 2>&1); then
  fail "another certificate Secret was accepted"
fi
echo "$out" | grep -q "cannot change while TLS is on" || fail "refused for another reason: $out"
echo "ok: another certificate Secret is refused"
