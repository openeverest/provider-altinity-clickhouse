#!/bin/sh
# Connects over both secure ports, verifying the server certificate against
# the CA from the connection details, and runs a distributed query between
# the replicas (secure native port), then checks that ClickHouse serves a
# renewed certificate without restarting.
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).
set -eu

NS=default
NAME=itest-ch-tls
POD="chi-$NAME-clickhouse-0-0-0"
PW=$(kubectl get secret "$NAME-credentials" -n "$NS" -o jsonpath='{.data.password}' | base64 -d)
CA_FILE=/tmp/itest-tls-ca.crt
CLIENT_CONFIG=/tmp/itest-tls-client.xml

in_pod() { kubectl exec -n "$NS" "$POD" -c clickhouse -- "$@"; }
write_in_pod() { kubectl exec -i -n "$NS" "$POD" -c clickhouse -- sh -c "cat > $1"; }

# Clients verify against the CA from the connection details.
kubectl get secret "$NAME-conn" -n "$NS" -o jsonpath='{.data.ca\.crt}' | base64 -d | write_in_pod "$CA_FILE"
write_in_pod "$CLIENT_CONFIG" <<EOF
<config>
    <openSSL>
        <client>
            <caConfig>$CA_FILE</caConfig>
            <verificationMode>strict</verificationMode>
            <invalidCertificateHandler>
                <name>RejectCertificateHandler</name>
            </invalidCertificateHandler>
        </client>
    </openSSL>
</config>
EOF

secure_query() {
  in_pod clickhouse-client --config-file "$CLIENT_CONFIG" --secure --host "clickhouse-$NAME" --port 9440 \
    --user admin --password "$PW" -q "$1"
}

served_serials() {
  in_pod sh -c "for t in localhost:8443 localhost:9440 chk-$NAME-keeper-keeper-0-0-client:2281; do echo | openssl s_client -connect \$t 2>/dev/null | openssl x509 -noout -serial; done" | sort -u
}

# Distributed queries need both replicas in the cluster definition first.
i=0
until [ "$(secure_query "SELECT count() FROM system.clusters WHERE cluster = 'clickhouse'" 2>/dev/null || echo 0)" = 2 ]; do
  i=$((i + 1))
  if [ "$i" -ge 36 ]; then echo "cluster did not register 2 replicas"; exit 1; fi
  sleep 5
done

ports=$(secure_query "SELECT groupUniqArray(port) FROM system.clusters WHERE cluster = 'clickhouse'")
if [ "$ports" != "[9440]" ]; then echo "remote_servers should use the secure port, got $ports"; exit 1; fi

replicas=$(secure_query "SELECT count() FROM clusterAllReplicas('clickhouse', system.one)")
if [ "$replicas" != 2 ]; then echo "distributed query over TLS reached $replicas replicas"; exit 1; fi
echo "native TLS (9440) verified; distributed query reached both replicas"

zk=$(secure_query "SELECT concat(host, ':', toString(port)) FROM system.zookeeper_connection")
case "$zk" in
  secure://*-client.*:2281) echo "Keeper over TLS: $zk" ;;
  *) echo "ClickHouse should reach Keeper over TLS, got $zk"; exit 1 ;;
esac

secure_query "CREATE TABLE IF NOT EXISTS default.itest_tls ON CLUSTER '{cluster}' (id UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/itest_tls', '{replica}') ORDER BY id" >/dev/null
secure_query "INSERT INTO default.itest_tls SELECT number FROM numbers(100)"
advertised=$(secure_query "SELECT value FROM system.zookeeper WHERE path = '/clickhouse/tables/0/itest_tls/replicas/chi-$NAME-clickhouse-0-0' AND name = 'host'")
case "$advertised" in
  *"port: 9010"*"scheme: https"*) echo "replicas fetch parts over HTTPS (9010)" ;;
  *) echo "replica 0 advertises: $advertised"; exit 1 ;;
esac
i=0
until [ "$(kubectl exec -n "$NS" "chi-$NAME-clickhouse-0-1-0" -c clickhouse -- clickhouse-client --user admin --password "$PW" -q 'SELECT count() FROM default.itest_tls' 2>/dev/null)" = 100 ]; do
  i=$((i + 1))
  if [ "$i" -ge 36 ]; then echo "rows did not replicate over HTTPS"; exit 1; fi
  sleep 5
done
echo "replication over HTTPS verified"

pong=$(in_pod wget -q -O- --ca-certificate="$CA_FILE" "https://clickhouse-$NAME.$NS.svc.cluster.local:8443/ping")
if [ "$pong" != "Ok." ]; then echo "HTTPS ping returned: $pong"; exit 1; fi
echo "HTTPS (8443) verified"

# Renewal: cert-manager re-issues the deleted Secret; the kubelet updates the
# mounted files, and ClickHouse and Keeper reload them without a restart.
before=$(served_serials)
restarts=$(kubectl get pod "$POD" -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')
kubectl delete secret "$NAME-server-tls" -n "$NS"
i=0
while :; do
  issued=$(kubectl get secret "$NAME-server-tls" -n "$NS" -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d 2>/dev/null |
    openssl x509 -noout -serial 2>/dev/null || true)
  if [ -n "$issued" ] && [ "$issued" != "$before" ] && [ "$(served_serials)" = "$issued" ]; then break; fi
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then echo "renewed certificate not served (issued: $issued, served: $(served_serials))"; exit 1; fi
  sleep 5
done
if [ "$(kubectl get pod "$POD" -n "$NS" -o jsonpath='{.status.containerStatuses[0].restartCount}')" != "$restarts" ]; then
  echo "ClickHouse restarted to pick up the renewed certificate"; exit 1
fi
secure_query "SELECT 1" >/dev/null
echo "renewed certificate served without a restart"
