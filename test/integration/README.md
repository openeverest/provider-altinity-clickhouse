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
- No `ClickHouseKeeperInstallation` is provisioned for it.

### `replicated`

Provisions a replicated (2-replica) Instance and asserts:

1. The Keeper is provisioned and `Completed`.
2. The CHI converges to `Completed` with 2 replicas.
3. A `ReplicatedMergeTree` table created on one replica, and a row inserted
   into it, appears on the other replica — proving the Keeper wiring works
   end-to-end, not just that both CRs reached `Completed`.

Each case cleans up its Instance in a `finally` block (the provider's own
finalizer logic garbage-collects the CHI/CHK).
