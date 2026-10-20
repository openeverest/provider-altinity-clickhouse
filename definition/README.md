# Provider Definition

This directory contains all the files you edit when creating or maintaining an
OpenEverest provider. Everything related to the provider's **identity**,
**versions**, **topologies**, **UI**, and **custom schemas** lives here.

> **For a complete development guide, see [PROVIDER_DEVELOPMENT.md](github.com/openeverest/provider-sdk/blob/main/PROVIDER_DEVELOPMENT.md).**

## Directory Layout

```
definition/
├── provider.yaml                    # Provider name + component→type mapping
├── versions.yaml                    # Component types and their version/image catalog
├── types.go                         # Shared types (e.g. TopologyType)
├── components/
│   └── types.go                     # Component custom spec types
└── topologies/
    └── /
        ├── topology.yaml            # Topology config + UI schema (co-located)
        └── types.go                 # Topology-specific config types
```

## Quick Reference

| File | Purpose | When to edit |
|------|---------|--------------|
| `provider.yaml` | Names the provider and maps logical component names to types. | When adding/removing a component. |
| `versions.yaml` | Lists available versions and container images per component type. | When adding operator releases. |
| `types.go` | Shared Go types used across the provider. | When adding provider-wide types. |
| `components/types.go` | Go structs for component custom specs. | When a component needs custom config. |
| `topologies/<name>/topology.yaml` | Defines a deployment topology and its UI rendering. | When adding a topology or changing its UI. |
| `topologies/<name>/types.go` | Go struct for topology-specific custom config. | When a topology needs custom config fields. |

## Adding Components and Topologies

Use the provider-sdk CLI to add components and topologies:

```bash
# Add a new component
provider-sdk add component --name backupAgent --type backup

# Add a new topology
provider-sdk add topology --name replicaSet
```

## How It All Fits Together

```
definition/ files
       │
       ▼
provider-sdk generate  ──▶  charts/<name>/generated/provider-spec.yaml
       │
       ▼
Helm chart template    ──▶  .Files.Get "generated/provider-spec.yaml"
```

Run `make generate` to regenerate all generated files.

## Keeping the catalog current

`versions.yaml` follows ClickHouse's release cycle: two LTS lines a year (`YY.3`, `YY.8`),
each supported for a year. On every check (at least monthly, and on each ClickHouse LTS
release or security advisory):

1. Find the latest patch of each supported LTS line, e.g.
   `curl -s "https://hub.docker.com/v2/repositories/clickhouse/clickhouse-server/tags?page_size=100&name=26.8." | jq -r '.results[].name'`.
   Use the full four-part tag; check the same tag exists for `clickhouse/clickhouse-keeper`.
2. Add the new versions to both component types and point the bundle at them. Moving an
   existing bundle to a newer patch is fine: running instances get it as a held
   maintenance action. Never move a bundle, or its Keeper, to an older version.
3. On a new LTS line, add a bundle for it (newest first), make it `defaultVersion`, and
   mark the versions of lines past end of support `deprecated: true`.
4. Never delete a version or bundle an Instance may still use: Instances keep their bundle
   name in `status.version`. To retire one, set `removedInVersion` to the provider release
   that drops it, so upgrading the provider is blocked while Instances still use it.
5. Run `make generate`, update the README versions table, and run the `upgrade`
   integration case.

