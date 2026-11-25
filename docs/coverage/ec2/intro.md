### Spot Instances Are a Mock

Spot Instance Requests launch on-demand instances and report them as `active` and `fulfilled`.
There is no spot market: no bidding, no price rejection and no interruption.

### AMI Launch Permissions Are Refused

An AMI cannot be shared with another account or made public.
`ModifyImageAttribute` and `ResetImageAttribute` accept only `description`, and refuse launch permission changes with `InvalidParameterValue`.
Terraform's `aws_ami_launch_permission` therefore cannot be created.

### RegisterImage Refuses Some Parameters

`ImageLocation`, `KernelId`, `RamdiskId`, `TpmSupport`, `ImdsSupport`, `SriovNetSupport` and any `VirtualizationType` other than `hvm` are refused with `InvalidParameterValue`.

`EnaSupport` is stored and returned, but does not limit which instance types can launch the image.

### EBS Encryption by Default Follows the Platform

The platform, not the account, decides whether new volumes are encrypted.
`GetEbsEncryptionByDefault` reports `true` when the platform encrypts volumes.
While it does, `DisableEbsEncryptionByDefault` is refused with `OperationNotPermitted`.
