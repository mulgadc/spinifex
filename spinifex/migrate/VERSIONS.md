# Config Versions

Single source of truth for the current schema version of every config file Spinifex installs. When you bump a template's version, update this table in the same change.

| Target | Current version | Canonical template |
|---|---|---|
| `nats.conf` | `3` | `cmd/spinifex/cmd/templates/nats.conf` |
| `awsgw.toml` | `2` | `cmd/spinifex/cmd/templates/awsgw.toml` |
| `spinifex.toml` | `4` | `cmd/spinifex/cmd/templates/spinifex.toml` |
| `predastore.toml` | `1` | `cmd/spinifex/cmd/templates/predastore.toml` |
| `predastore-multinode.toml` | `1` | `cmd/spinifex/cmd/templates/predastore-multinode.toml` |

## How versions are stamped

1. **Fresh install (`spx admin init`)** — the version string is baked into the embedded template file and written verbatim to disk. There is no Go constant; the template is the source of truth.
2. **Upgrade (`spx admin upgrade`)** — the migration framework (`Registry.RunConfig`) calls `ConfigVersionReader.WriteVersion` after each registered migration step. A target with no registered migration is left alone, and the upgrade command reports nothing pending for it.

## Registered config migrations

| Target | Migration | File |
|---|---|---|
| `spinifex.toml` | `3` → `4`: rename `[nodes.*.predastore]` `node_id` → `host_id` | `spinifex/migrate/005_spinifex_predastore_host_id.go` |

## Registered object-store migrations

These migrate data in Predastore rather than a config file. Predastore has no conditional write, so their version is stamped in a shared JetStream KV bucket instead of in an object; see `RunObject`.

| Target | Migration | File | Version bucket |
|---|---|---|---|
| `ebsmetadata` | `0` → `1`: backfill `spinifex/ebsmetadata/v1` documents from legacy viperblock `config.json` | `spinifex/migrate/ebsmetadatabackfill/006_ebsmetadata_backfill.go` | `spinifex-ebsmetadata-migrate` |

Unlike config migrations, these do not run from `spx admin upgrade`. The ebsmetadata backfill runs from `Daemon.configureEBSProvider`, and only when `[ebs] provider = "viperblockd"` is selected: backfilling while the embedded engine is still authoritative would produce documents that immediately go stale.

## Where migrations live

Register a `ConfigMigration` against `DefaultRegistry` in a new numbered file under `spinifex/migrate/`, and bump the version in both the template and the table above.

## Framework

The framework (`migrate.go`, `version_readers.go`) covers three kinds of target:

- `RunKV` is called from service-startup paths to stamp NATS KV bucket versions.
- `RunConfig` / `RunAllConfig` / `PendingConfig` back `spx admin upgrade`, which `scripts/setup.sh` runs with `--yes` while the services are stopped.
- `RunObject` migrates object-store data, stamping its version in a caller-supplied JetStream KV bucket. The stamp is a read-then-write, not a compare-and-swap, so two nodes can race to run the same step — every `ObjectMigration.Run` must be safe to re-execute.
