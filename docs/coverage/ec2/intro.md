### Spot Instances Are a Mock

Spot Instance Requests are a mock over the on-demand `RunInstances` path. A request synchronously launches real VMs on the operator's own compute and is then reported `active` and `fulfilled`. There is no spot market: no bidding, no price rejection, no interruption and no reclamation, and instances are never taken back.

### AMI Launch Permissions Are Refused

An AMI cannot be shared with another account or made public. `ModifyImageAttribute` refuses `LaunchPermission`, `UserIds`, `UserGroups`, `OrganizationArns`, `OrganizationalUnitArns` and `OperationType`, and `ResetImageAttribute` resets only `description`, not `launchPermission`. Each refusal is `InvalidParameterValue`. Terraform's `aws_ami_launch_permission` therefore cannot be created. `description` is the only image attribute that can be modified or reset.

### RegisterImage Registers From a Snapshot

`RegisterImage` takes a root EBS snapshot in `BlockDeviceMappings` and writes an image that points at it.
`EnaSupport` is supported, with `true` and `false` both accepted and stored on the image.
`DescribeImages` returns `EnaSupport` when the image has one and leaves the member out when it was never set, as AWS does.
`CopyImage` carries the value to the copy, and `CreateImage` sets it to `true` because every instance reports ENA support.
`RunInstances` does not read the flag, so an image registered with `EnaSupport=false` is not held back from instance types that need ENA on AWS, since guests use virtio networking either way.

`RegisterImage` refuses `ImageLocation`, `KernelId`, `RamdiskId`, `TpmSupport`, `ImdsSupport` and `SriovNetSupport`, and any `VirtualizationType` other than `hvm`.
Each refusal is `InvalidParameterValue`, and the message names the parameter and the value, for example `Value (aki-123) for parameter KernelId is invalid. KernelId is not supported.`
The same form is used for a bad `Architecture`, `BootMode` or `Name` length.
A missing `Name`, `BlockDeviceMappings` or root snapshot answers `MissingParameter` and names what is missing.
Registering with `--ena-support`, `--no-ena-support` and neither, copying the image, and three refusals were run on a three-node cluster with the AWS CLI.
Launching an instance from an `EnaSupport` image was not run, so the launch path is unchanged by construction and not by test.
