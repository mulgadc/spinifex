# Config Versions

Single source of truth for the current schema version of every config file Spinifex installs. When you bump a template's version, update this table in the same change.

| Target | Current version | Canonical template |
|---|---|---|
| `nats.conf` | `3` | `cmd/spinifex/cmd/templates/nats.conf` |
| `awsgw.toml` | `2` | `cmd/spinifex/cmd/templates/awsgw.toml` |
| `spinifex.toml` | `4` | `cmd/spinifex/cmd/templates/spinifex.toml` |
| `predastore.toml` | `5` | `cmd/spinifex/cmd/templates/predastore.toml` |
| `predastore-multinode.toml` | `5` | `cmd/spinifex/cmd/templates/predastore-multinode.toml` |

## How versions are stamped

1. **Fresh install (`spx admin init`)** — the version string is baked into the embedded template file and written verbatim to disk. There is no Go constant; the template is the source of truth.
2. **Upgrade (`spx admin upgrade`)** — the migration framework (`Registry.RunConfig`) calls `ConfigVersionReader.WriteVersion` after each registered migration step. A target with no registered migration is left alone, and the upgrade command reports nothing pending for it.

## Registered config migrations

| Target | Migration | File |
|---|---|---|
| `predastore.toml` | `4` → `5`: `[[db]]`/`[[nodes]]` → `[[host]]`/`[[node]]`, relocate node data, recover raft | `spinifex/migrate/predastoretopology/004_predastore_topology.go` |
| `spinifex.toml` | `3` → `4`: rename `[nodes.*.predastore]` `node_id` → `host_id` | `spinifex/migrate/005_spinifex_predastore_host_id.go` |

## Where migrations live

Register a `ConfigMigration` against `DefaultRegistry` in a new numbered file under `spinifex/migrate/`, and bump the version in both the template and the table above.

A migration that needs more than the framework and the standard library goes in its own package beside `migrate` instead, blank-imported from `cmd/spinifex/cmd/admin_upgrade.go`. `migrate` is imported by every handler that stamps a KV bucket version, so a heavy dependency there reaches the whole tree — `predastoretopology` needs Predastore's cluster runtime, which is why it sits apart.

## Framework

The framework (`migrate.go`, `version_readers.go`) covers both kinds of target:

- `RunKV` is called from service-startup paths to stamp NATS KV bucket versions.
- `RunConfig` / `RunAllConfig` / `PendingConfig` back `spx admin upgrade`, which `scripts/setup.sh` runs with `--yes` while the services are stopped.
