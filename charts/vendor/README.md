# Vendored charts

`altinity-clickhouse-operator/` is the Altinity clickhouse-operator Helm chart
from the unreleased `0.28.0` branch, commit `92bb6b683`
(`deploy/helm/clickhouse-operator`), which matches the published
`altinity/clickhouse-operator:0.28.0` image. It brings the operator-managed
backup CRDs (`ClickHouseBackup`, `ClickHouseBackupSchedule`,
`ClickHouseRestore`) the provider's backups need.

Remove it and point `Chart.yaml` back at `https://helm.altinity.com/` once
Altinity publishes 0.28.0.

Refresh: `git -C <clickhouse-operator> archive <commit> deploy/helm/clickhouse-operator | tar -x`
and move `deploy/helm/clickhouse-operator` here.
