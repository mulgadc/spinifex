package handlers_rds

import (
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
	// Empty means "resolve the account's default VPC subnet", matching AWS's own
	// behaviour when a request names no subnet group.
	DBSubnetGroupName    string
	DBParameterGroupName string
	DeletionProtection   bool
	// Inert, and stored only so a describe echoes back what the request set: the
	// engine version is pinned, so nothing upgrades either way. AWS defaults it
	// to true and so does the Terraform provider, so an unset request must not
	// read back as false.
	AutoMinorVersionUpgrade bool
	// Automated backups as they will be in force: the retention defaulted when the
	// request names none, and both windows in canonical text — assigned from the
	// instance identifier when unnamed, never left empty (rds-9).
	BackupRetentionPeriod      int64
	PreferredBackupWindow      string
	PreferredMaintenanceWindow string
	Tags                       map[string]string
}

// Everything that can be decided from the request alone. Network resolution
// runs afterwards, so a malformed request never reaches the VPC.
func (s *Service) validateCreateRequest(input *rds.CreateDBInstanceInput) (*validatedCreate, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
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
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBInstanceClass %q is not supported; supported classes are %s", instanceClass, strings.Join(SupportedInstanceClasses(), ", "))
	}

	storage := aws.Int64Value(input.AllocatedStorage)
	if storage < minAllocatedStorageGiB || storage > maxAllocatedStorageGiB {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"AllocatedStorage must be between %d and %d GiB", minAllocatedStorageGiB, maxAllocatedStorageGiB)
	}

	storageType := strings.ToLower(strings.TrimSpace(aws.StringValue(input.StorageType)))
	if storageType == "" {
		storageType = storageTypeGP3
	}
	if storageType != storageTypeGP3 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"StorageType %q is not supported; only %q is offered", storageType, storageTypeGP3)
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
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"Port must be between %d and %d", minDBPort, maxDBPort)
		}
	}

	// Both groups are resolved against KV by the caller, which is the only place
	// they can be: this validates the request alone. An unnamed parameter group
	// takes the engine's implicit default, matching AWS.
	paramGroup := aws.StringValue(input.DBParameterGroupName)
	if paramGroup == "" {
		paramGroup = engine.DefaultParameterGroupName()
	}

	// Automated backups are on by default at the full retention cap, matching the
	// console's own default. A request naming 0 explicitly still turns them off.
	retention := s.defaultRetentionDays()
	if input.BackupRetentionPeriod != nil {
		retention = aws.Int64Value(input.BackupRetentionPeriod)
		if err := s.validateRetentionPeriod(retention); err != nil {
			return nil, err
		}
	}
	backupWindow, maintenanceWindow, err := s.validateWindows(identifier,
		aws.StringValue(input.PreferredBackupWindow), aws.StringValue(input.PreferredMaintenanceWindow))
	if err != nil {
		return nil, err
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
		DBSubnetGroupName:    aws.StringValue(input.DBSubnetGroupName),
		DBParameterGroupName: paramGroup,
		DeletionProtection:   aws.BoolValue(input.DeletionProtection),

		AutoMinorVersionUpgrade: input.AutoMinorVersionUpgrade == nil || aws.BoolValue(input.AutoMinorVersionUpgrade),

		BackupRetentionPeriod:      retention,
		PreferredBackupWindow:      backupWindow,
		PreferredMaintenanceWindow: maintenanceWindow,

		Tags: tags,
	}, nil
}

// AWS's own identifier rules. Enforcing them here keeps the identifier usable
// as a DNS label, which D6 makes it.
func validateDBInstanceIdentifier(id string) error {
	switch {
	case id == "":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBInstanceIdentifier is required")
	case len(id) > maxDBInstanceIdentifierLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBInstanceIdentifier must be at most %d characters", maxDBInstanceIdentifierLen)
	case !isLetter(rune(id[0])):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBInstanceIdentifier must begin with a letter")
	case strings.HasSuffix(id, "-"):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBInstanceIdentifier may not end with a hyphen")
	case strings.Contains(id, "--"):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBInstanceIdentifier may not contain consecutive hyphens")
	}
	for _, r := range id {
		if !isDigit(r) && r != '-' && (r < 'a' || r > 'z') {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"DBInstanceIdentifier may contain only lowercase letters, digits and hyphens")
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
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is not supported: %s", parameter, why)
}
