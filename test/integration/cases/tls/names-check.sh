#!/bin/sh
# Adds a name to spec.parameters.tls.additionalNames of the running Instance:
# cert-manager reissues the certificate, and ClickHouse and Keeper serve it
# without any pod being replaced or restarted.
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).
set -eu
NAME=itest-ch-tls
. ../../lib/clickhouse.sh

pods() {
  kubectl get pods -n "$NS" -l "clickhouse.altinity.com/chi=$NAME" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}/{.status.containerStatuses[0].restartCount}{"\n"}{end}' || return 1
  kubectl get pods -n "$NS" -l "clickhouse-keeper.altinity.com/chk=$NAME-keeper" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}/{.status.containerStatuses[0].restartCount}{"\n"}{end}' || return 1
}

# served <host:port>: the DNS names of the certificate served there.
served() {
  kubectl exec -n "$NS" "$(pod 0)" -c clickhouse -- sh -c \
    "echo | openssl s_client -connect $1 2>/dev/null | openssl x509 -noout -ext subjectAltName"
}
serves_new_name() { served "$1" | grep -q "DNS:analytics.company.com"; }

before=$(pods)
kubectl patch instances.core.openeverest.io "$NAME" -n "$NS" --type=merge \
  -p '{"spec":{"parameters":{"tls":{"additionalNames":["itest-ch-tls.company.com","analytics.company.com"]}}}}'

for target in "chi-$NAME-clickhouse-0-0:8443" "chi-$NAME-clickhouse-0-1:9440" "chk-$NAME-keeper-keeper-0-0-client:2281"; do
  wait_until "$target serves a certificate for analytics.company.com" serves_new_name "$target"
done
served "chi-$NAME-clickhouse-0-0:8443" | grep -q "DNS:itest-ch-tls.company.com" || fail "the first additional name was dropped"
wait_until "the Instance is Ready" instance_ready

after=$(pods)
[ "$before" = "$after" ] || fail "pods were replaced or restarted for the new name:
before:
$before
after:
$after"
echo "additional name served without a restart"
