### Spot Instances Are a Mock

Spot Instance Requests are a mock over the on-demand `RunInstances` path. A request synchronously launches real VMs on the operator's own compute and is then reported `active` and `fulfilled`. There is no spot market: no bidding, no price rejection, no interruption and no reclamation, and instances are never taken back.

### AMI Launch Permissions Are Refused

An AMI cannot be shared with another account or made public. `ModifyImageAttribute` refuses `LaunchPermission`, `UserIds`, `UserGroups`, `OrganizationArns`, `OrganizationalUnitArns` and `OperationType`, and `ResetImageAttribute` resets only `description`, not `launchPermission`. Each refusal is `InvalidParameterValue`. Terraform's `aws_ami_launch_permission` therefore cannot be created. `description` is the only image attribute that can be modified or reset.
