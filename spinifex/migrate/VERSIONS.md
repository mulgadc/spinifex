# Config Versions

Single source of truth for the current schema version of every config file Spinifex installs. When you bump a template's version, update this table in the same change.

| Target | Current version | Canonical template |
|---|---|---|
| `nats.conf` | `3` | `cmd/spinifex/cmd/templates/nats.conf` |
| `awsgw.toml` | `2` | `cmd/spinifex/cmd/templates/awsgw.toml` |
| `spinifex.toml` | `2` | `cmd/spinifex/cmd/templates/spinifex.toml` |
| `predastore.toml` | `4` | `cmd/spinifex/cmd/templates/predastore.toml` |
| `predastore-multinode.toml` | `4` | `cmd/spinifex/cmd/templates/predastore-multinode.toml` |

## How versions are stamped

1. **Fresh install (`spx admin init`)** — the version string is baked into the embedded template file and written verbatim to disk. There is no Go constant; the template is the source of truth.
2. **Upgrade (`spx admin upgrade`)** — the migration framework (`Registry.RunConfig`) calls `ConfigVersionReader.WriteVersion` after each registered migration step. With no migrations registered, the upgrade command reports "No pending config migrations" and exits cleanly.

## Current state

**No config migrations are currently registered.** Schema bumps ship via force-reinstall. To restore in-place upgrade for a given target, register a `ConfigMigration` against `DefaultRegistry` in a new file under `spinifex/migrate/` and bump the version in both the template and this document.

## Framework

The framework (`migrate.go`, `version_readers.go`) is intentionally preserved:

- `RunKV` is still called from service-startup paths to stamp NATS KV bucket versions.
- `RunConfig` / `RunAllConfig` / `PendingConfig` remain available so a future migration can register cleanly without re-introducing the framework.
