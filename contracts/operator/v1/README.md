# Operator v1 contracts

This package owns versioned cross-process interfaces for deployment
administration. It is separate from AWS tenant-facing service contracts.

`StorageConfigSubject` supplies the Predastore topology consumed by SPX
`GetStorageStatus`. Its response deliberately contains Predastore-specific
roles and Reed-Solomon configuration. It is an operational status contract,
not a generic provider abstraction; do not change its subject or JSON shape
incompatibly. Add a new version when that is required.
