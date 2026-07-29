package handlers_rds

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// AWS's PostgreSQL range. The upper bound is not a platform limit — it is the
// point past which the customer is asking for something no engine build here
// has been exercised at.
const (
	minAllocatedStorageGiB = 20
	maxAllocatedStorageGiB = 65536

	// The only storage type offered. Every other AWS type names a performance
	// class this platform does not implement, so accepting one would promise
	// IOPS that are not delivered.
	storageTypeGP3 = "gp3"

	// The port range AWS accepts. Below 1150 collides with the well-known
	// range the guest's own services use.
	minDBPort = 1150
	maxDBPort = 65535

	maxDBInstanceIdentifierLen = 63
)

// The request as CreateDBInstance resolved it: defaults filled in, every
// unimplemented parameter already rejected.
type validatedCreate struct {
	Identifier       string
	Engine           Engine
	EngineVersion    string
	InstanceClass    string
	InstanceType     string
	AllocatedStorage int64
	StorageType      string
	Port             int64
	MasterUsername   string
	MasterPassword   string
	DBName           string
	SecurityGroupIDs []string
	// Empty means "resolve the account's default VPC subnet"; rds-7 replaces
	// this with a real DB subnet group lookup.
	SubnetID             string
	DBParameterGroupName string
	Tags                 map[string]string
}

// Everything that can be decided from the request alone. Network resolution
// runs afterwards, so a malformed request never reaches the VPC.
func validateCreateRequest(input *rds.CreateDBInstanceInput) (*validatedCreate, error) {
	if input == nil {
		return nil, fmt.Errorf("%s: empty request", awserrors.ErrorInvalidParameterValue)
	}
	if err := rejectUnimplemented(input); err != nil {
		return nil, err
	}

	identifier := aws.StringValue(input.DBInstanceIdentifier)
	if err := validateDBInstanceIdentifier(identifier); err != nil {
		return nil, err
	}

	engine, err := LookupEngine(aws.StringValue(input.Engine))
	if err != nil {
		return nil, err
	}
	if err := engine.ValidateVersion(aws.StringValue(input.EngineVersion)); err != nil {
		return nil, err
	}

	instanceClass := aws.StringValue(input.DBInstanceClass)
	instanceType, err := InstanceTypeForClass(instanceClass)
	if err != nil {
		return nil, fmt.Errorf("%s: DBInstanceClass %q is not supported; supported classes are %s",
			awserrors.ErrorInvalidParameterValue, instanceClass, strings.Join(SupportedInstanceClasses(), ", "))
	}

	storage := aws.Int64Value(input.AllocatedStorage)
	if storage < minAllocatedStorageGiB || storage > maxAllocatedStorageGiB {
		return nil, fmt.Errorf("%s: AllocatedStorage must be between %d and %d GiB",
			awserrors.ErrorInvalidParameterValue, minAllocatedStorageGiB, maxAllocatedStorageGiB)
	}

	storageType := strings.ToLower(strings.TrimSpace(aws.StringValue(input.StorageType)))
	if storageType == "" {
		storageType = storageTypeGP3
	}
	if storageType != storageTypeGP3 {
		return nil, fmt.Errorf("%s: StorageType %q is not supported; only %q is offered",
			awserrors.ErrorInvalidParameterValue, storageType, storageTypeGP3)
	}

	masterUsername := aws.StringValue(input.MasterUsername)
	if err := engine.ValidateMasterUsername(masterUsername); err != nil {
		return nil, err
	}
	masterPassword := aws.StringValue(input.MasterUserPassword)
	if err := ValidateMasterUserPassword(masterPassword); err != nil {
		return nil, err
	}

	port := engine.DefaultPort
	if input.Port != nil {
		port = aws.Int64Value(input.Port)
		if port < minDBPort || port > maxDBPort {
			return nil, fmt.Errorf("%s: Port must be between %d and %d",
				awserrors.ErrorInvalidParameterValue, minDBPort, maxDBPort)
		}
	}

	// No subnet groups exist until rds-7, so a named one cannot resolve. Saying
	// so is the honest answer; silently placing the ENI somewhere else would put
	// the endpoint in a subnet the customer did not choose.
	if group := aws.StringValue(input.DBSubnetGroupName); group != "" {
		return nil, fmt.Errorf("%s: DB subnet group %q not found", awserrors.ErrorDBSubnetGroupNotFound, group)
	}

	// The implicit default group is the one name that will resolve once rds-7
	// materialises it, so accepting it now keeps a Terraform config that names
	// it working across both phases.
	paramGroup := aws.StringValue(input.DBParameterGroupName)
	if paramGroup != "" && paramGroup != engine.DefaultParameterGroupName() {
		return nil, fmt.Errorf("%s: DB parameter group %q not found", awserrors.ErrorDBParameterGroupNotFound, paramGroup)
	}

	// Rejected before the identifier is reserved, so a create with bad tags
	// leaves no partial record behind.
	tags, err := validateTags(input.Tags)
	if err != nil {
		return nil, err
	}

	return &validatedCreate{
		Identifier:           identifier,
		Engine:               engine,
		EngineVersion:        engine.EngineVersion(),
		InstanceClass:        instanceClass,
		InstanceType:         instanceType,
		AllocatedStorage:     storage,
		StorageType:          storageType,
		Port:                 port,
		MasterUsername:       masterUsername,
		MasterPassword:       masterPassword,
		DBName:               aws.StringValue(input.DBName),
		SecurityGroupIDs:     aws.StringValueSlice(input.VpcSecurityGroupIds),
		DBParameterGroupName: engine.DefaultParameterGroupName(),
		Tags:                 tags,
	}, nil
}

// AWS's own identifier rules. Enforcing them here keeps the identifier usable
// as a DNS label, which D6 makes it.
func validateDBInstanceIdentifier(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%s: DBInstanceIdentifier is required", awserrors.ErrorInvalidParameterValue)
	case len(id) > maxDBInstanceIdentifierLen:
		return fmt.Errorf("%s: DBInstanceIdentifier must be at most %d characters",
			awserrors.ErrorInvalidParameterValue, maxDBInstanceIdentifierLen)
	case !isLetter(rune(id[0])):
		return fmt.Errorf("%s: DBInstanceIdentifier must begin with a letter", awserrors.ErrorInvalidParameterValue)
	case strings.HasSuffix(id, "-"):
		return fmt.Errorf("%s: DBInstanceIdentifier may not end with a hyphen", awserrors.ErrorInvalidParameterValue)
	case strings.Contains(id, "--"):
		return fmt.Errorf("%s: DBInstanceIdentifier may not contain consecutive hyphens", awserrors.ErrorInvalidParameterValue)
	}
	for _, r := range id {
		if !isDigit(r) && r != '-' && (r < 'a' || r > 'z') {
			return fmt.Errorf("%s: DBInstanceIdentifier may contain only lowercase letters, digits and hyphens",
				awserrors.ErrorInvalidParameterValue)
		}
	}
	return nil
}

// D19: a supported action carrying an unimplemented parameter must not silently
// drop it. Each rejection below is a parameter whose omission would create a
// false safety, security or availability guarantee. Parameters that are merely
// inert — AutoMinorVersionUpgrade, Performance Insights, Enhanced Monitoring,
// CopyTagsToSnapshot — are deliberately absent and accepted as no-ops.
func rejectUnimplemented(input *rds.CreateDBInstanceInput) error {
	if aws.BoolValue(input.MultiAZ) {
		return unimplemented("MultiAZ", "this platform is single-AZ; a standby would not exist")
	}
	if aws.BoolValue(input.PubliclyAccessible) {
		return unimplemented("PubliclyAccessible",
			"the endpoint is a private VPC address; a public one would not be reachable")
	}
	// Rejected in both directions: false asks for unencrypted storage, which is
	// not offered, and omitting it entirely still yields encrypted storage.
	if input.StorageEncrypted != nil && !aws.BoolValue(input.StorageEncrypted) {
		return unimplemented("StorageEncrypted=false", "unencrypted storage is not offered")
	}
	if aws.BoolValue(input.DeletionProtection) {
		return unimplemented("DeletionProtection", "DeleteDBInstance does not honour it yet")
	}
	if aws.Int64Value(input.BackupRetentionPeriod) > 0 {
		return unimplemented("BackupRetentionPeriod", "automated backups are not implemented yet")
	}
	if aws.StringValue(input.PreferredBackupWindow) != "" {
		return unimplemented("PreferredBackupWindow", "automated backups are not implemented yet")
	}
	if aws.StringValue(input.PreferredMaintenanceWindow) != "" {
		return unimplemented("PreferredMaintenanceWindow", "maintenance windows are not implemented yet")
	}
	if aws.BoolValue(input.EnableIAMDatabaseAuthentication) {
		return unimplemented("EnableIAMDatabaseAuthentication", "IAM database authentication is not implemented")
	}
	if aws.Int64Value(input.Iops) > 0 {
		return unimplemented("Iops", "provisioned IOPS are not implemented; storage is gp3")
	}
	if aws.StringValue(input.KmsKeyId) != "" {
		return unimplemented("KmsKeyId", "storage is encrypted with the cluster key, not a customer-managed one")
	}
	if aws.StringValue(input.AvailabilityZone) != "" {
		return unimplemented("AvailabilityZone", "this platform exposes a single zone")
	}
	if len(input.DBSecurityGroups) > 0 {
		return unimplemented("DBSecurityGroups",
			"EC2-Classic security groups are not offered; use VpcSecurityGroupIds")
	}
	if aws.StringValue(input.DBClusterIdentifier) != "" {
		return unimplemented("DBClusterIdentifier", "clustered engines are not offered")
	}
	if len(input.EnableCloudwatchLogsExports) > 0 {
		return unimplemented("EnableCloudwatchLogsExports", "log export is not implemented")
	}
	return nil
}

func unimplemented(parameter, why string) error {
	return fmt.Errorf("%s: %s is not supported: %s", awserrors.ErrorInvalidParameterValue, parameter, why)
}
