# RDS engine catalogue: boundary and evidence

The engine catalogue is static, process-local metadata: engine identity and versions, parameter-group families, the parameter catalogue and effective-parameter resolution, and `db.*` instance classes.
It has no durable state and no realization, so it has no ADR-0003 lifecycle contract; this note records its boundaries and the behaviour pinned before it moves.
Paths are relative to `spinifex/` unless they start with `tests/`.

## Pinned behaviour

`handlers/rds/engine_catalogue_characterization_test.go`, alongside the existing `engine_test.go`, `engine_mariadb_test.go`, `enginefamily_test.go`, `paramcatalog_test.go`, `sizing_test.go` and `catalog_test.go`:

- Family lookup ignores case and surrounding whitespace; an unknown family, a product name such as `PostgreSQL`, and an unsupported version return exactly `InvalidParameterValue` (`TestEngineForFamily_NormalisesCaseAndWhitespace`, `TestEngineForFamily_RejectsUnknownFamily`, `TestLookupEngine_RejectsProductNameWithExactCode`, `TestEngineValidateVersion_UnsupportedVersionCarriesExactCode`).
- Master password rejections resolve to `InvalidParameterValue` with fixed messages (`TestValidateMasterUserPassword_ExactCodesAndMessages`).
- Per engine and parameter, the option-file name, data type and static or dynamic apply type the guest agent relies on (`TestCatalogueAgentContractMetadata_MatchesGoldenTable`).
- Internal faults return `ServerInternal`: an engine with no combination check, and a combination check reading a key the catalogue does not define (`TestResolveEffectiveParameters_NoCombinationCheckIsServerInternal`, `TestResolveEffectiveParameters_CombinationCheckMissingKeyIsServerInternal`).
- The exact unclean-stop and crash-consistent snapshot recovery text per engine (`TestUncleanStopMessage_PinsExactTextPerEngine`, `TestCrashConsistentSnapshotMessage_PinsExactTextPerEngine`).
- The reference class for reported parameter defaults is `db.t3.micro` at 1024 MiB (`TestSmallestInstanceClass_MemoryIsExactlyOneGiB`).

## Discovered and corrected divergence

`ValidateMasterUserPassword` built its empty-password error with a bare `errors.New`, which `awserrors` does not resolve, so `CreateDBInstance` without `MasterUserPassword` returned HTTP 500 `ServerInternal` where AWS returns a 400 client error.
It now returns 400 `InvalidParameterValue`; `gateway/rds_test.go` `TestRDSRequest_CreateDBInstance_EmptyMasterUserPasswordReturns400` covers the gateway, NATS and RDS service path.

## Boundaries the owner move must keep

1. EC2 instance-type memory reaches the catalogue only through an immutable `Sizing` value built from a catalogue-declared `InstanceTypes` interface, backed by the fixed `domains/ec2/instancetypes` table at composition. The catalogue does not import an EC2 package, and `Sizing` is a static projection, never a route to live node availability; `DescribeOrderableDBInstanceOptions` keeps its caller-supplied runnable check in the gateway.
2. The guest agent consumes engine metadata through a narrow interface and input/output shapes it declares itself, with an engine-backed implementation supplied by `cmd/rds-agent`. The agent does not import the catalogue package; if that injection cannot be added without a larger composition design, its existing `handlers/rds` dependency stays as recorded debt.
3. Effective parameters leave the catalogue as an engine-owned `Setting{Name, Value}`, converted to `handlers/rds.Parameter` at the adapter; `Parameter` is not moved or aliased.
4. AWS output building (`describeVersion`, `orderableOption`), paging, `ApplyMethod*`/`ParameterSource*`, the gateway runnable check and the storage limits stay in the existing adapter path.
