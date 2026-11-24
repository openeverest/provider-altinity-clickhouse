#!/bin/sh
# What a running Instance with TLS cannot change is refused at admission:
# turning TLS off and another certificate source.
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).
set -eu
NAME=itest-ch-tls
. ../../lib/clickhouse.sh

# refused <description> <expected message> <json merge patch>
refused() {
  if out=$(kubectl patch instances.core.openeverest.io "$NAME" -n "$NS" --type=merge -p "$3" 2>&1); then
    fail "$1 was accepted"
  fi
  echo "$out" | grep -q "$2" || fail "$1 was refused for another reason: $out"
  echo "ok: $1 is refused"
}

refused "turning TLS off" "TLS cannot be turned off" \
  '{"spec":{"parameters":{"tls":{"mode":"disabled","additionalNames":null}}}}'
refused "an issuer instead of the provider's CA" "cannot change while TLS is on" \
  '{"spec":{"parameters":{"tls":{"issuer":{"name":"company-ca","kind":"ClusterIssuer"}}}}}'
refused "a certificate Secret instead of the provider's CA" "cannot change while TLS is on" \
  '{"spec":{"parameters":{"tls":{"certificateSecretName":"mine","additionalNames":null}}}}'
