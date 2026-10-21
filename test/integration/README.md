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
2. Moves it back to `26.3` and asserts the downgrade is rejected: the CHI stays
   on 26.8.

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

### `monitoring`

Installs a minimal `PodMonitor` CRD, provisions a standalone Instance with
`podMonitor: enabled`, and asserts:

1. The CHI bakes the native Prometheus endpoint into `spec.configuration.settings`.
2. The provider creates a `PodMonitor` selecting the instance's ClickHouse pods
   on the `metrics` port.
3. Setting `podMonitor: disabled` removes the `PodMonitor`.

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

The CI cluster does not enforce NetworkPolicies, so blocking the plaintext
ports is only asserted through the policies themselves.

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
   rejects the change (`Failed`, the CHI untouched), and reverting the size
   recovers the Instance.

### `topology-change`

Switches a standalone Instance with a `MergeTree` table to the replicated
topology: Keeper is provisioned and the CHI is reapplied with two replicas
wired to it. The table keeps its rows on the first replica, its schema
reaches the new replica, and rows copied into a `ReplicatedMergeTree` table
replicate.

The day-2 cases share helpers in [`lib/clickhouse.sh`](lib/clickhouse.sh).

Each case cleans up its Instance in a `finally` block (the provider's own
finalizer logic garbage-collects the CHI/CHK).
