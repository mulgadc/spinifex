# Operator v1 contracts

This package owns versioned cross-process interfaces for deployment
administration. It is separate from AWS tenant-facing service contracts.

`StorageConfigSubject` supplies the Predastore topology consumed by SPX
`GetStorageStatus`. Its response deliberately contains Predastore-specific
roles and Reed-Solomon configuration. It is an operational status contract,
not a generic provider abstraction; do not change its subject or JSON shape
incompatibly. Add a new version when that is required.

`admin.go` holds the `/admin/<Method>` request and response bodies the private admin surface shares with the CLI.
`AdminMethodNames` is the list of callable methods, and the gateway router builds its accept set from it.
`AccountQuotaRequest`, `AccountQuotaResponse` and `DeleteAccountResponse` stay in the gateway until the `/admin` handlers move, because they embed types the contracts layer may not import.
Do not change a field name, JSON tag or omitempty incompatibly; add a new version when that is required.
