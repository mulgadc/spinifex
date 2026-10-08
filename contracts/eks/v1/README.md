# EKS v1 contracts

This package owns the deployed EKS add-on contract between the control-plane guest (the on-VM addon-sync agent, `eks-gateway-fetch` and `eks-gateway-publish`) and the host (the AWS gateway and the cluster reconciler). It is not an AWS tenant-facing API.

## Delivery report

`AddonStatusSubject` retains the deployed `eks.addon.<account>.<cluster>.status` route. The guest's `mulga-eks-addon-sync.sh` builds an `AddonStatusReport` with `printf`, so it always sends `message` (possibly empty); the gateway relays the payload verbatim on the `addon` channel of `internal-publish`. `AddonDeliveryPhase` is the closed set `applied`, `ready`, `failed`.

## Staged manifests

`InternalAddonsResponse` is the body of `GET /clusters/{name}/internal-addons/{account}`. The gateway renders it with the AWS SDK REST-JSON marshaller (`jsonutil.BuildJSON`), which ignores `json` tags, so the deployed body uses Go field names and always includes empty strings, for example `{"Addons":[{"AddonName":"x","AddonVersion":"1","ServiceAccountRoleArn":"","ConfigurationValues":""}]}`. The guest decodes it with `encoding/json`, which matches keys case-insensitively, then emits it as tab-separated lines for the shell agent.

The `json` tags on `StagedAddonManifest` are the host-internal NATS form used between the gateway and the daemon (`eks.ListStagedAddonManifests`). They do not describe the guest-facing body.

## Compatibility boundary

The subject, phase values, both JSON forms and Go field names are wire compatibility; `addon_status_test.go` and `addon_manifest_test.go` pin them with literal bytes. A generation-aware or otherwise incompatible delivery protocol needs a successor version; it is not a v1 change.
