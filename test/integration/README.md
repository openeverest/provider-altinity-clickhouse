# Integration tests (chainsaw)

End-to-end tests that run against a real Kubernetes cluster with the OpenEverest
core, this provider, and the bundled Altinity ClickHouse operator installed.

## Prerequisites

- A running cluster with OpenEverest core + this provider deployed. The easiest
  way is the local dev stack:

  ```bash
  make dev-up        # k3d cluster + OpenEverest core + provider (Tilt)
  ```

- [`chainsaw`](https://kyverno.github.io/chainsaw/) on your `PATH`:

  ```bash
  go install github.com/kyverno/chainsaw@v0.2.15
  ```

## Run

```bash
make test-integration
```

This runs `chainsaw test test/integration/cases --config test/integration/chainsaw-config.yaml`.
Run a single case with `make test-integration INTEGRATION_CASES=test/integration/cases/<case>`;
CI runs every case this way, each in its own k3d cluster, on every PR.
Tests run in the fixed `default` namespace (matching how the dev stack installs
the provider), not chainsaw's default ephemeral per-test namespace.

## Cases

### `standalone`

Provisions a standalone (single-replica) Instance and asserts:

- The CHI converges to `Completed` with a single replica.
- Custom engine `configuration` propagates to the CHI's
  `spec.configuration.settings`.
- No `ClickHouseKeeperInstallation` is provisioned for it.
- Changing the memory limit of the running cluster reports the Instance as
  `Updating` while the pod rolls, then `Ready` again with the CHI at the new
  limit.

### `replicated`

Provisions a replicated (2-replica) Instance and asserts:

1. The Keeper is provisioned and `Completed`.
2. The CHI converges to `Completed` with 2 replicas.
3. Custom engine `configuration` propagates to the CHI's
   `spec.configuration.settings`.
4. A `ReplicatedMergeTree` table created on one replica, and a row inserted
   into it, appears on the other replica — proving the Keeper wiring works
   end-to-end, not just that both CRs reached `Completed`.
5. Without a `schedulingPolicy`, both the CHI and the CHK pod templates carry
   the default preferred pod anti-affinity (hostname and zone), and both volume
   claim templates carry the default `Retain` reclaim policy.

### `retention`

Provisions a standalone Instance with the default `storageReclaimPolicy` and:

1. Writes rows, deletes the Instance and asserts the data PVC is retained.
2. Recreates the Instance under the same name and asserts the rows are back.
3. Switches `storageReclaimPolicy` to `Delete`, deletes the Instance and asserts
   the PVC is removed.

### `upgrade`

Provisions a standalone Instance on the `26.3` bundle, writes rows, then:

1. Moves it to `26.8` and asserts it reports `Updating`, then `Ready` on the
   26.8 image, with the rows intact and `version()` reporting 26.8.
2. Moves it back to `26.3` and asserts admission rejects the downgrade: the
   CHI stays on 26.8.

### `expose`

Provisions a standalone Instance with a `LoadBalancer` Service and asserts the
Altinity operator creates the root `clickhouse-<name>` Service with the
requested `type`, annotations, and `loadBalancerSourceRanges`. The connection
Secret's `host` and `uri` then point at the load balancer address, with
`internal_host` keeping the in-cluster Service. Switching to a `NodePort`
Service publishes `http_port_node_port` and `native_port_node_port`.

### `scheduling`

Provisions a standalone Instance with an engine `schedulingPolicy` and asserts
the node selector, tolerations, affinity and topology spread constraints land
on the CHI pod template and the running pod. The spread constraint is declared
without a selector, so the provider scopes it to the engine pods' labels.

### `presets`

Asserts the chart installs the `clickhouse-standalone` and
`clickhouse-replicated` `ClusterInstancePreset`s, that both are admitted as
Instance specs (server-side dry run through the provider's admission webhook),
and that an Instance created from the standalone preset becomes `Ready`. The
replicated preset is only dry-run: its resources do not fit a CI runner.

### `monitoring`

Claims a test `MonitoringClass` that requires metrics by hand (no extension runs),
seeds the `PodMonitor` earlier releases created, provisions a standalone Instance
listing a destination of the class (with the deprecated `podMonitor: enabled`), and
asserts:

1. Core reports the class compatible with the provider: `ExtensionManaged`, no
   disruption, since ClickHouse serves metrics natively.
2. Core creates the `MonitoringBinding`, and the provider publishes the engine's
   endpoint (`metrics` port, `/metrics`, kind `clickhouse`) in
   `status.monitoring.sources`.
3. The endpoint serves ClickHouse metrics (scraped through the API server's pod proxy).
4. The legacy `PodMonitor` is deleted and none is created.
5. Dropping the entry deletes the binding and keeps the published endpoint.

### `user`

Provisions a standalone Instance seeded with credentials through
`spec.userSecretRef` (`dba` / a known password) and asserts:

1. The `<name>-credentials` Secret holds the seeded username and password, the
   password digest, and the generated credentials for the `default` user,
   replication fetches and the cluster secret.
2. The CHI references every password from that Secret (by digest or environment
   variable), configures `interserver_http_credentials` and the cluster secret,
   and the seeded user can log in while a password-less `default` login fails.
3. Editing `password` in the credentials Secret rotates it: after the pod rolls
   the new password works, the old one is rejected, and the connection Secret
   carries the new password.

### `tls`

Provisions a replicated Instance with `tls.enabled: enabled` (required mode by
default) and asserts (needs cert-manager, installed by the dev stack):

1. cert-manager issues the server certificate into the `<name>-server-tls`
   `kubernetes.io/tls` Secret, covering the client and replica Services.
2. The CHI serves HTTPS `8443` and native `9440`, its cluster is `secure`,
   replicas fetch parts over HTTPS (`9010`), and ClickHouse is wired to Keeper's
   client Services on `2281` over TLS. The CHK is TLS-only and mounts the same
   certificate. The client Service carries only the secure ports; the
   `<name>-plaintext` and `<name>-keeper-access` NetworkPolicies limit the
   plaintext ports and Keeper; the connection Secret reports port `8443` and
   the CA.
3. Clients connect over `9440` and `8443` verifying the certificate against
   that CA, a distributed query reaches both replicas over TLS, ClickHouse's
   Keeper session runs over TLS, and rows replicate over HTTPS.
4. After the certificate Secret is re-issued, ClickHouse and Keeper serve the
   new certificate without restarting.
5. Switching to `mode: optional` restores the plaintext ports on the client
   Service and removes both NetworkPolicies.
6. Turning TLS off (`enabled: disabled`) leaves only the plaintext ports on the
   client Service, and ClickHouse reaches Keeper on `2181`. Once no pod mounts
   them, the server and CA certificates, their Secrets and the issuers are
   deleted. The connection Secret reports `http` on `8123` without a CA, and
   replication and a distributed query work over the plaintext ports.

The CI cluster does not enforce NetworkPolicies, so blocking the plaintext
ports is only asserted through the policies themselves.

### `upgrade-replicated`

Provisions a replicated Instance (2 replicas, 3 Keeper nodes) on the `26.3`
bundle and starts a client pod that inserts a row a second through the client
Service. Moving it to `26.8` reports `Updating`, then `Ready` with ClickHouse
and Keeper on the 26.8 images, and:

1. Writes never stop for more than 60 seconds while the nodes roll one at a
   time.
2. Both replicas run 26.8, hold the same rows, and none of the rows the client
   saw committed is missing.

### `provider-upgrade`

Upgrades the provider chart from the last published release to this checkout,
the way users do:

1. Moves the provider's Helm release to the published chart and image (the
   latest version in `oci://ghcr.io/openeverest/charts`; set `PUBLISHED_VERSION`
   to start from an older one).
2. Creates a replicated Instance and writes rows.
3. Runs `helm upgrade` back to this checkout's chart and image, with the
   preflight hook on.
4. For two minutes the Instance stays `Ready` and no ClickHouse or Keeper pod is
   replaced: a change the new release would roll out is held as pending
   maintenance.
5. Approves whatever is held, waits for it to roll out, and checks the rows
   and replication.

A step cleanup restores this build if the case fails. The case swaps the
provider out from under the cluster, so it does not run alongside the others.

### `scale`

Scales a replicated Instance from 2 to 3 replicas, back to 2 and to 3 again:

1. The new replica gets the `ReplicatedMergeTree` table and its rows.
2. With the default `Retain` policy, the removed replica keeps its PVC and its
   Keeper registration (3 total, 2 active), and writes continue.
3. Scaled back up, it rejoins and catches up on the rows written meanwhile.

### `day2`

Day-2 operations on a replicated Instance:

1. A `configuration` change rolls out to both replicas (the operator restarts
   ClickHouse one replica at a time).
2. A deleted ClickHouse pod comes back and catches up on rows written while
   it was down.
3. Inserts succeed while a Keeper pod is down, and Keeper recovers.
4. Growing `storage.size` expands the volumes when their StorageClass allows
   it. Otherwise, as with the CI cluster's `local-path` class, the provider
   blocks the Instance (`Ready` condition with reason `InvalidReference`, the
   CHI untouched), and reverting the size recovers the Instance.

### `topology-change`

Switches a standalone Instance with a `MergeTree` table to the replicated
topology: Keeper is provisioned and the CHI is reapplied with two replicas
wired to it. The table keeps its rows on the first replica, its schema
reaches the new replica, and rows copied into a `ReplicatedMergeTree` table
replicate.

### `admission`

Exercises the provider's admission webhook with a standalone Instance asking
for two replicas:

1. The write is rejected, with `--dry-run=server` and for real, naming
   `spec.components[engine].replicas`.
2. With the provider scaled to 0 it is admitted, and once the provider is back
   the Instance's `Ready` condition reports `ValidationFailed` with that error.
3. An update adding a CPU limit below 1 is rejected for that limit only, while
   an update changing the memory limit alone is admitted.
4. With one replica the provider creates the `ClickHouseInstallation`.
5. An Instance naming a missing `spec.userSecretRef` Secret is admitted with a
   warning.

The case scales the provider down, so it does not run alongside the others.

### `sharding`

Provisions a replicated Instance with `numShards: 2` and one replica per shard:

1. The CHI has 2 shards of 1 replica and is wired to Keeper.
2. Rows inserted through a `Distributed` table over a `ReplicatedMergeTree`
   table spread over both shards.
3. Raising `numShards` to 3: the new shard gets both tables, keeps no
   existing rows, reads every row through the `Distributed` table, and new
   rows reach it.
4. Lowering `numShards` is rejected at admission, and the CHI keeps 3 shards.

The day-2 cases share helpers in [`lib/clickhouse.sh`](lib/clickhouse.sh).

Each case cleans up its Instance in a `finally` block (the provider's own
finalizer logic garbage-collects the CHI/CHK).
