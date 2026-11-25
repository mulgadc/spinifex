### TLS is required on MariaDB

MariaDB requires encrypted connections by default (`require_secure_transport=1`), but AWS leaves this off.
Set it to `0` in a parameter group to permit plaintext connections without a reboot.

### Rejected parameters

These parameters are rejected with `InvalidParameterValue`, so that a feature that is not available is never silently ignored.

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
| `EngineVersion` other than the engine's pin; on modify, any `EngineVersion` and `Engine` | No in-place engine or version change |
| `Engine=mysql` (and Aurora engines) | Oracle MySQL is not offered; `mariadb` is a distinct engine, not an alias for it |
| `NewDBInstanceIdentifier` | Renaming an instance is not supported |
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
| `DBInstanceAutomatedBackupsArn` (DescribeDBInstanceAutomatedBackups) | Cross-region automated backup replication is not offered, so no replicated backup has an ARN |
| `dbi-resource-id` filter (DescribeDBSnapshots, DescribeDBInstanceAutomatedBackups) | Snapshots and automated backups do not record their source instance's resource ID |
