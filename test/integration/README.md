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
Tests run in the fixed `default` namespace (matching how the dev stack installs
the provider), not chainsaw's default ephemeral per-test namespace.

## Cases

### `standalone`

Provisions a standalone (single-replica) Instance and asserts:

- The CHI converges to `Completed` with a single replica.
- Custom engine `configuration` propagates to the CHI's
  `spec.configuration.settings`.
- No `ClickHouseKeeperInstallation` is provisioned for it.

### `replicated`

Provisions a replicated (2-replica) Instance and asserts:

1. The Keeper is provisioned and `Completed`.
2. The CHI converges to `Completed` with 2 replicas.
3. Custom engine `configuration` propagates to the CHI's
   `spec.configuration.settings`.
4. A `ReplicatedMergeTree` table created on one replica, and a row inserted
   into it, appears on the other replica — proving the Keeper wiring works
   end-to-end, not just that both CRs reached `Completed`.

### `expose`

Provisions a standalone Instance with a `LoadBalancer` Service and asserts the
Altinity operator creates the root `clickhouse-<name>` Service with the
requested `type`, annotations, and `loadBalancerSourceRanges`.

### `monitoring`

Installs a minimal `PodMonitor` CRD, provisions a standalone Instance with
`podMonitor: enabled`, and asserts:

1. The CHI bakes the native Prometheus endpoint into `spec.configuration.settings`.
2. The provider creates a `PodMonitor` selecting the instance's ClickHouse pods
   on the `metrics` port.
3. Setting `podMonitor: disabled` removes the `PodMonitor`.

### `user`

Provisions a standalone Instance and asserts the provider creates the
application `admin` user:

1. A `<name>-credentials` Secret holds the username (`admin`), a random
   password, and its SHA256 digest (64 hex chars).
2. The CHI provisions the `admin` user and references the password digest from
   the credentials Secret, so the plaintext never lands in the ClickHouse config.

### `tls`

Provisions a standalone Instance with `tls.enabled: enabled` and asserts (needs
cert-manager, installed by the dev stack):

1. cert-manager issues the server certificate into the `<name>-server-tls`
   `kubernetes.io/tls` Secret.
2. The CHI adds the secure ports (HTTPS `8443`, native `9440`) and mounts the
   certificate files **additively** — the plaintext ports remain available.

Each case cleans up its Instance in a `finally` block (the provider's own
finalizer logic garbage-collects the CHI/CHK).
