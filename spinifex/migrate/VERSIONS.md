# Config Versions

Single source of truth for the current schema version of every config file Spinifex installs. When you bump a template's version, update this table in the same change.

| Target | Current version | Canonical template |
|---|---|---|
| `nats.conf` | `3` | `cmd/spinifex/cmd/templates/nats.conf` |
| `awsgw.toml` | `3` | `cmd/spinifex/cmd/templates/awsgw.toml` |
| `spinifex.toml` | `4` | `cmd/spinifex/cmd/templates/spinifex.toml` |
| `predastore.toml` | `1` | `cmd/spinifex/cmd/templates/predastore.toml` |
| `predastore-multinode.toml` | `1` | `cmd/spinifex/cmd/templates/predastore-multinode.toml` |

## How versions are stamped

1. **Fresh install (`spx admin init`)** — the version string is baked into the embedded template file and written verbatim to disk. There is no Go constant; the template is the source of truth.
2. **Upgrade (`spx admin upgrade`)** — the migration framework (`Registry.RunConfig`) calls `ConfigVersionReader.WriteVersion` after each registered migration step. A target with no registered migration is left alone, and the upgrade command reports nothing pending for it.

## Registered migrations

None. The migrations that used to live here predated a breaking change that required `spx admin init --force`, so no install can reach the current versions by migrating and the steps were dropped rather than left as dead code.

`spinifex.toml` is still registered as a config target in `migrate.go` so `spx admin upgrade` reports its on-disk version. KV and object-store buckets have no registered migrations either, so `RunKV` and `RunObject` stamp their target version directly on first init.

## Where migrations live

Register a `ConfigMigration` against `DefaultRegistry` in a new numbered file under `spinifex/migrate/`, and bump the version in both the template and the table above.

There are no migrations in the tree to copy from. For worked examples of all three kinds, read the deleted files out of git history:

```bash
git log --diff-filter=D --name-only -- 'spinifex/migrate/0*.go'
git show <commit>^:spinifex/migrate/003_ipam_purpose.go
```

## Framework

The framework (`migrate.go`, `version_readers.go`) covers three kinds of target:

- `RunKV` is called from service-startup paths to stamp NATS KV bucket versions.
- `RunConfig` / `RunAllConfig` / `PendingConfig` back `spx admin upgrade`, which `scripts/setup.sh` runs with `--yes` while the services are stopped.
- `RunObject` migrates object-store data, stamping its version in a caller-supplied JetStream KV bucket. The stamp is a read-then-write, not a compare-and-swap, so two nodes can race to run the same step — every `ObjectMigration.Run` must be safe to re-execute.
