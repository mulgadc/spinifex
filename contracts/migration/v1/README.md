# Migration contract, version 1

This package owns the migration domain's account-termination participant
declaration. The engine in `operator/accountlifecycle` imports it from here,
not from `spinifex/domains/migration`.

The participant name is `migration`. Its owned classes are the record kinds of
the per-account bucket `spinifex-migration-<account>`: `connection`,
`assessment` and `copy-task`. The domain serves the routes of
`contracts/accountlifecycle/v1` under that name. Raise `DeclarationVersion`
whenever the classes change.
