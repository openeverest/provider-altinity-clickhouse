# provider-altinity-clickhouse

An [OpenEverest](https://github.com/openeverest) provider for [ClickHouse](https://clickhouse.com),
built on the [Altinity Kubernetes Operator for ClickHouse](https://github.com/Altinity/clickhouse-operator).

## Prerequisites

- Go 1.26+
- Kubernetes cluster (k3d, kind, or remote)
- [OpenEverest](https://github.com/openeverest/openeverest) installed

The [Altinity ClickHouse Operator](https://github.com/Altinity/clickhouse-operator)
ships as a bundled Helm dependency (subchart) and is installed automatically with
the provider chart — no separate install step is required. The operator chart
manages its own CRDs via pre-install/pre-upgrade hooks.

> **k3d / kind users:** The Altinity operator uses inotify heavily. The default
> `fs.inotify.max_user_instances=128` causes silent reconciliation failures.
> Bump it before installing:
> ```bash
> echo "fs.inotify.max_user_instances = 8192" | sudo tee /etc/sysctl.d/99-k8s.conf
> sudo sysctl --system
> ```

## Supported Topologies

| Topology     | Description                                              | Status       |
|--------------|----------------------------------------------------------|--------------|
| `standalone` | Single-node ClickHouse, no ZooKeeper                     | ✅ Available |
| `replicated` | Multi-replica with ClickHouse Keeper (3-node Raft quorum)| ✅ Available |

### standalone

Single ClickHouse node. No coordination dependency. Suitable for development,
analytics workloads, and read-heavy pipelines (e.g. CDC sinks).

### replicated

Multi-replica ClickHouse with built-in [ClickHouse Keeper](https://clickhouse.com/docs/en/guides/sre/keeper/clickhouse-keeper)
for distributed coordination. No ZooKeeper required.

Architecture:
- **ClickHouseKeeperInstallation (CHK):** 3 replicas, Raft quorum — always provisioned automatically
- **ClickHouseInstallation (CHI):** configurable replicas (minimum 2), single shard

Supports `ReplicatedMergeTree` and all replicated table engines.

## Supported Versions

| Version | Type   | Default |
|---------|--------|---------|
| `25.3`  | LTS    | ✅ Yes  |
| `24.8`  | LTS    |         |
| `25.5`  | Latest |         |

## Installation

The provider chart is published as an OCI artifact to the GitHub Container
Registry. It bundles the Altinity ClickHouse operator as a subchart, so a single
install brings up both the provider and the operator.

```bash
helm install provider-altinity-clickhouse \
  oci://ghcr.io/openeverest/charts/provider-altinity-clickhouse \
  --version 0.1.0 \
  --create-namespace
```

Upgrade to a newer chart version:

```bash
helm upgrade provider-altinity-clickhouse \
  oci://ghcr.io/openeverest/charts/provider-altinity-clickhouse \
  --version 0.1.0
```

Uninstall:

```bash
helm uninstall provider-altinity-clickhouse
```

> Browse available versions on the
> [chart package page](https://github.com/openeverest/provider-altinity-clickhouse/pkgs/container/charts%2Fprovider-altinity-clickhouse).

## Quick Start

```bash
# Generate all manifests (RBAC, provider spec, Helm chart)
make generate

# Run the provider locally against your cluster
make run

# Or deploy via Helm
make helm-install
```

## Development

### Project Structure

```
cmd/provider/              # Entry point
internal/
  provider/
    provider.go            # ProviderInterface implementation (Validate/Sync/Status/Cleanup)
    rbac.go                # Kubebuilder RBAC markers
  common/
    spec.go                # Component name constants
definition/
  provider.yaml            # Provider name + component→type mapping
  versions.yaml            # Component type version/image catalog
  types.go                 # Shared Go types
  components/
    types.go               # Component custom spec types
  topologies/
    standalone/
      topology.yaml        # Single-node topology config + UI schema
    replicated/
      topology.yaml        # Multi-replica topology config + UI schema
config/
  rbac/
    role.yaml              # Generated ClusterRole (do not edit manually)
charts/provider-altinity-clickhouse/     # Helm chart for deployment
  generated/
    rbac-rules.yaml        # Generated RBAC rules (do not edit manually)
    provider-spec.yaml     # Generated Provider CR spec (do not edit manually)
examples/
  instance-example.yaml    # Example Instance CR (replicated)
  instance-simple.yaml     # Minimal Instance CR (standalone)
dev/
  k3d_config.yaml          # Local k3d cluster config
Makefile                   # Build, generate, and deploy targets
```

### Make Targets

| Target                  | Description                                                |
|-------------------------|-------------------------------------------------------------|
| `make generate`         | Run all code generation (RBAC + Helm sync + provider spec) |
| `make run`              | Run the provider locally                                   |
| `make build`            | Build the provider binary                                  |
| `make docker-build`     | Build the container image                                  |
| `make helm-install`     | Deploy with Helm                                           |
| `make helm-template`    | Render Helm templates locally (dry-run)                    |
| `make test`             | Run unit tests                                             |
| `make test-integration` | Run kuttl integration tests                                |
| `make verify`           | Check generated files are up-to-date (CI)                  |
| `make lint`             | Run golangci-lint                                          |

> For development patterns (RBAC, watches, code generation), see
> [PROVIDER_DEVELOPMENT.md](https://github.com/openeverest/provider-sdk/blob/main/PROVIDER_DEVELOPMENT.md).

### Known Gotcha — mergo replace directive

The Altinity operator depends on a fork of mergo. Add this to `go.mod`:

```
replace github.com/imdario/mergo => github.com/sunsingerus/mergo v0.3.12
```

## Deployment

### Helm (from source)

For local development you can install the chart directly from the checked-out
source tree instead of the published OCI artifact:

```bash
# Install
helm install provider-altinity-clickhouse charts/provider-altinity-clickhouse/ --create-namespace

# Upgrade
helm upgrade provider-altinity-clickhouse charts/provider-altinity-clickhouse/

# Uninstall
helm uninstall provider-altinity-clickhouse
```

> For installing a published release, see [Installation](#installation).

### Local Development (k3d)

```bash
# Create a local k3d cluster
make k3d-cluster-up

# Run the provider locally against the cluster
make run

# Run integration tests
make test-integration

# Tear down the cluster
make k3d-cluster-down
```

## Related

- [openeverest/openeverest#1763](https://github.com/openeverest/openeverest/issues/1763) — ClickHouse support tracking issue
- [openeverest/openeverest#2339](https://github.com/openeverest/openeverest/issues/2339) — chproxy managed proxy support
- [Altinity/clickhouse-operator#2007](https://github.com/Altinity/clickhouse-operator/pull/2007) — Helm watchNamespaces fix

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
