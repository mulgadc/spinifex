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

## KV bucket versions

| Bucket | Current version | Constant |
|---|---|---|
| `spinifex-instance-state` | `5` | `daemon.InstanceStateBucketVersion` |
| `spinifex-cluster-state` | `1` | `daemon.ClusterStateBucketVersion` |
| `spinifex-terminated-instances` | `3` | `daemon.TerminatedInstanceBucketVersion` |

## Registered migrations

**Config:** none. The migrations that used to live here predated a breaking change that required `spx admin init --force`, so no install can reach the current versions by migrating and the steps were dropped rather than left as dead code. `spinifex.toml` is still registered as a config target in `migrate.go` so `spx admin upgrade` reports its on-disk version.

**KV:** `spinifex-instance-state` and `spinifex-terminated-instances` are both migrated from `daemon/instance_records_migrate.go`, onto the per-resource instance record key space.

| Bucket | Step | What it does |
|---|---|---|
| `spinifex-instance-state` | `1`→`2` | copy the `instance.<id>` keys onto the record space |
| `spinifex-instance-state` | `2`→`3` | split each `node.<id>` blob, one record per instance in it |
| `spinifex-instance-state` | `3`→`4` | re-key the record space from `i/<id>` to `i.<id>` |
| `spinifex-instance-state` | `4`→`5` | seed a `nodepresence.<id>` marker for every node that has a `node.<id>` blob |
| `spinifex-terminated-instances` | `1`→`2` | copy the `terminated.<id>` keys onto the record space |
| `spinifex-terminated-instances` | `2`→`3` | re-key the record space from `i/<id>` to `i.<id>` |

The re-key steps exist because a watch filter is a NATS subject filter: `*` matches one dot-delimited token, so `i/<id>` is a single token and `i/*` matches nothing. Only a build that shipped the slash has anything for them to move.

Version `5` of `spinifex-instance-state` is the cutover: the record space became the only copy, and `instance.<id>` and `node.<id>` are frozen — read by nothing, written by nothing, and left in place so the crossing can be rolled back. The bump is also what a build predating the cutover trips over: `RunKV` refuses a bucket stamped past what it understands, with `ErrSchemaAhead`, rather than opening it and reading half a key space.

Every other bucket has no registered migration, so `RunKV` stamps its target version directly on first init.

**Object store:** none, so `RunObject` stamps directly too.

## Where migrations live

Register a `ConfigMigration` against `DefaultRegistry` in a new numbered file under `spinifex/migrate/`, and bump the version in both the template and the table above.

A KV migration goes in the package that owns the bucket instead, next to the constant it bumps — `migrate` cannot import the record types it would have to decode. `daemon/instance_records_migrate.go` is the worked example.

For worked examples of the config and object-store kinds, read the deleted files out of git history:

```bash
git log --diff-filter=D --name-only -- 'spinifex/migrate/0*.go'
git show <commit>^:spinifex/migrate/003_ipam_purpose.go
```

## Framework

The framework (`migrate.go`, `version_readers.go`) covers three kinds of target:

- `RunKV` is called from service-startup paths to stamp NATS KV bucket versions.
- `RunConfig` / `RunAllConfig` / `PendingConfig` back `spx admin upgrade`, which `scripts/setup.sh` runs with `--yes` while the services are stopped.
- `RunObject` migrates object-store data, stamping its version in a caller-supplied JetStream KV bucket. The stamp is a read-then-write, not a compare-and-swap, so two nodes can race to run the same step — every `ObjectMigration.Run` must be safe to re-execute.
