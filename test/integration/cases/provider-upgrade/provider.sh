#!/bin/sh
# Helpers for the provider-upgrade case: find the provider's Helm release and
# move it between the last published chart and this checkout's chart.
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).

STATE=${STATE:-/tmp/itest-provider-upgrade}
CHART_OCI=${CHART_OCI:-oci://ghcr.io/openeverest/charts/provider-altinity-clickhouse}
CHART_DIR=$(git rev-parse --show-toplevel)/charts/provider-altinity-clickhouse

# release: "<name> <namespace>" of the provider's Helm release.
release() {
  helm list -A -o json |
    jq -r '[.[] | select(.chart | test("^provider-altinity-clickhouse-[0-9]"))][0] | "\(.name) \(.namespace)"'
}

# provider_deploy: "<namespace> <name>" of the provider Deployment.
provider_deploy() {
  set -- $(release)
  echo "$2 $(kubectl get deploy -n "$2" -l app.kubernetes.io/name=provider-altinity-clickhouse,app.kubernetes.io/instance="$1" \
    -o jsonpath='{.items[0].metadata.name}')"
}

# provider_image: the image the provider Deployment runs.
provider_image() {
  set -- $(provider_deploy)
  kubectl get deploy -n "$1" "$2" -o jsonpath='{.spec.template.spec.containers[0].image}'
}

# wait_provider: waits for the provider Deployment to roll out.
wait_provider() {
  set -- $(provider_deploy)
  kubectl rollout status deploy -n "$1" "$2" --timeout=5m
}

# pod_uids: the ClickHouse and Keeper pods of the Instance, as "name=uid" lines.
pod_uids() {
  kubectl get pods -n "$NS" -l "clickhouse.altinity.com/chi=$NAME" -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}'
  kubectl get pods -n "$NS" -l "clickhouse-keeper.altinity.com/chk=$NAME-keeper" -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}'
}
