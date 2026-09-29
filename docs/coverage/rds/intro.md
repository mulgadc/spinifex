### Engines

Spinifex offers PostgreSQL and MariaDB. Each DB instance is one dedicated system-owned VM running the engine directly, launched from a platform AMI and hidden from the customer's EC2 API. `Engine` is fixed at create: there is no in-place engine change, no cross-engine snapshot restore and no migration between the two.

`mysql` is not an accepted engine and is not an alias for `mariadb`. MariaDB is offered under its own AWS engine name, exactly as AWS RDS offers it, so a client — including Terraform's `aws_db_instance` — must set `engine = "mariadb"`.

Engine versions are pinned per engine. An `EngineVersion` naming anything but the pin is rejected, including a narrower minor version, because the AMI makes no promise about which minor it carries.

### The endpoint is private

The engine is reached over a customer-account ENI injected into a subnet of the DB subnet group, so the endpoint is reachable from inside the VPC only. The DB VM has other NICs that no customer security group governs, and the engine binds none of them — the port is not open there at all, rather than open and gated.

### TLS is required by default

Both engines enforce encrypted connections by default: `rds.force_ssl` on PostgreSQL and `require_secure_transport` on MariaDB. For MariaDB this is a deliberate divergence from AWS, which leaves it off. Both are boolean, modifiable and dynamic, so setting either to `0` in a parameter group restores plaintext without a reboot.

### Rejected parameters

A parameter whose omission would create a false safety, security or availability guarantee is rejected with `InvalidParameterValue` rather than silently dropped.

| Parameter | Why it is rejected |
|-----------|--------------------|
| `MultiAZ=true` | Single-AZ platform; a standby would not exist |
| `PubliclyAccessible=true` | The endpoint is a private VPC address |
| `StorageEncrypted=false` | Unencrypted storage is not offered |
| `EnableIAMDatabaseAuthentication` | IAM database authentication is not implemented |
| `Iops`, `StorageThroughput`, `StorageType` ≠ `gp3` | Provisioned performance classes are not implemented |
| `KmsKeyId`, `TdeCredentialArn` | Storage is encrypted with the cluster key, not a customer-managed one |
| `AvailabilityZone` | The platform exposes a single zone |
| `AvailabilityZoneGroup` (orderable options) | It selects a zone or local-zone group, and naming a zone is already refused |
| `DBSecurityGroups` | EC2-Classic security groups — use `VpcSecurityGroupIds` |
| `DBClusterIdentifier`, `DBClusterSnapshotIdentifier` | Clustered engines are not offered |
| `EnableCloudwatchLogsExports` | Log export is not implemented |
| `EngineVersion` other than the engine's pin, `Engine` on modify | No in-place engine or version change |
| `Engine=mysql` (and Aurora engines) | Oracle MySQL is not offered; `mariadb` is a distinct engine, not an alias for it |
| `NewDBInstanceIdentifier` | The identifier is the DNS label and the KV key |
| `DBPortNumber`, `DBSubnetGroupName` on modify | Both would move the endpoint |
| `MaxAllocatedStorage` | Storage autoscaling is not implemented |
| `ManageMasterUserPassword`, `RotateMasterUserPassword` | Secrets Manager integration is not offered |
| `CACertificateIdentifier` | The serving certificate is minted from the cluster CA |
| `Domain`, `DomainFqdn` | Active Directory domain join is not offered |
| `OptionGroupName` | Option groups are not offered |
| `CustomIamInstanceProfile` | The DB VM's instance profile is platform-owned |
| `EnableCustomerOwnedIp` | An Outposts feature |
| `ForceFailover` (reboot) | No standby to fail over to |
| `DBSnapshotIdentifier` (stop) | Snapshot-on-stop is not implemented |
| `dbi-resource-id` filter (DescribeDBSnapshots, DescribeDBInstanceAutomatedBackups) | Snapshots and automated backups do not record their source instance's resource ID |
