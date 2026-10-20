#!/bin/sh
# Checks that the connection Secret reports how to reach the exposed Instance:
# the LoadBalancer address as host (internal_host keeps the in-cluster
# Service), or the node ports when exposed through a NodePort.
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).
set -eu

NS=default
NAME=itest-ch-expose
MODE=$1

conn() { kubectl get secret "$NAME-conn" -n "$NS" -o jsonpath="{.data.$1}" 2>/dev/null | base64 -d 2>/dev/null || true; }
svc() { kubectl get service "clickhouse-$NAME" -n "$NS" -o jsonpath="$1"; }

if [ "$MODE" = loadbalancer ]; then
  # Clusters without a LoadBalancer controller (the CI cluster runs without
  # servicelb) never assign an address: play the controller and set one.
  i=0
  until [ -n "$(svc '{.status.loadBalancer.ingress[0].hostname}{.status.loadBalancer.ingress[0].ip}')" ]; do
    i=$((i + 1))
    if [ "$i" -ge 6 ]; then
      kubectl patch service "clickhouse-$NAME" -n "$NS" --subresource=status --type=merge \
        -p '{"status":{"loadBalancer":{"ingress":[{"ip":"203.0.113.10"}]}}}'
      break
    fi
    sleep 5
  done
fi

i=0
while :; do
  case "$MODE" in
    loadbalancer)
      want=$(svc '{.status.loadBalancer.ingress[0].hostname}')
      [ -n "$want" ] || want=$(svc '{.status.loadBalancer.ingress[0].ip}')
      got=$(conn host)
      if [ -n "$want" ] && [ "$got" = "$want" ]; then
        case "$(conn uri)" in
          *"@$want:8123/") ;;
          *) echo "uri does not point at $want"; exit 1 ;;
        esac
        break
      fi
      ;;
    nodeport)
      want="$(svc '{.spec.ports[?(@.port==8123)].nodePort}')/$(svc '{.spec.ports[?(@.port==9000)].nodePort}')"
      got="$(conn http_port_node_port)/$(conn native_port_node_port)"
      if [ "$got" = "$want" ] && [ "$want" != "/" ]; then break; fi
      ;;
  esac
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then echo "$MODE: connection details report $got, expected $want"; exit 1; fi
  sleep 5
done

internal=$(conn internal_host)
if [ "$internal" != "clickhouse-$NAME.$NS.svc.cluster.local" ]; then
  echo "internal_host is $internal"; exit 1
fi
if [ "$(conn http_port)/$(conn native_port)" != "8123/9000" ]; then
  echo "unexpected ports: $(conn http_port)/$(conn native_port)"; exit 1
fi
echo "$MODE: connection details report $got (internal: $internal)"
