#!/bin/sh
# Helpers for the day-2 integration cases. Source it from a case script with
# NAME (the Instance) set; NS defaults to "default".
#
# POSIX sh only: chainsaw runs scripts with /bin/sh (dash on Debian).

NS=${NS:-default}

# pod <replica index>: the ClickHouse pod of a replica (single shard).
pod() { echo "chi-$NAME-clickhouse-0-$1-0"; }

# q <replica index> <sql>: runs a query as the admin user.
q() {
  kubectl exec -n "$NS" "$(pod "$1")" -c clickhouse -- clickhouse-client \
    --user admin --password "$(kubectl get secret "$NAME-credentials" -n "$NS" -o jsonpath='{.data.password}' | base64 -d)" \
    -q "$2"
}

# wait_until <description> <command...>: retries for up to 10 minutes.
wait_until() {
  desc=$1
  shift
  i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 120 ]; then
      echo "timed out waiting for: $desc"
      return 1
    fi
    sleep 5
  done
  echo "ok: $desc"
}

# equals <expected> <command...>: succeeds when the command prints expected.
equals() {
  want=$1
  shift
  [ "$("$@" 2>/dev/null)" = "$want" ]
}

phase() { kubectl get instances.core.openeverest.io "$NAME" -n "$NS" -o jsonpath='{.status.phase}'; }
chi() { kubectl get clickhouseinstallations.clickhouse.altinity.com "$NAME" -n "$NS" -o jsonpath="$1"; }
ready() { [ "$(phase)" = Ready ] && [ "$(chi '{.status.status}')" = Completed ]; }

# patch_instance <json merge patch>: remembers the task ID of the applied
# ClickHouseInstallation, which the provider renews with every change.
patch_instance() {
  BEFORE_TASK=$(chi '{.spec.taskID}')
  kubectl patch instances.core.openeverest.io "$NAME" -n "$NS" --type=merge -p "$1" >/dev/null
}

# rolled_out: the provider applied a new ClickHouseInstallation, the operator
# finished reconciling it, and the Instance is Ready. (The CHI status keeps
# reading Completed for the previous one until the operator picks it up.)
rolled_out() {
  task=$(chi '{.spec.taskID}')
  [ -n "$task" ] && [ "$task" != "$BEFORE_TASK" ] &&
    chi '{.status.taskIDsCompleted}' | grep -qF "\"$task\"" &&
    [ "$(phase)" = Ready ]
}

# wait_ready: waits for the change made with patch_instance to roll out.
wait_ready() { wait_until "the change rolled out and the Instance is Ready" rolled_out; }

# replicas_in_cluster: hosts in the ClickHouse cluster definition.
replicas_in_cluster() { q 0 "SELECT count() FROM system.clusters WHERE cluster = 'clickhouse'"; }

fail() {
  echo "$*"
  exit 1
}
