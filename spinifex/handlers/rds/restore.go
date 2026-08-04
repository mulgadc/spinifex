package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// Builds a new DB instance on a volume created from the snapshot. It is a fresh
// instance in every respect but its data: a new identifier, customer ENI, DNS
// name, serving certificate and VM. What it inherits is the datadir — which
// already holds the database, its roles and their password hashes, so the engine
// starts on it rather than running initdb.
func (s *Service) RestoreDBInstanceFromDBSnapshot(ctx context.Context, input *rds.RestoreDBInstanceFromDBSnapshotInput, accountID string) (out *rds.RestoreDBInstanceFromDBSnapshotOutput, err error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	if err := validateDBInstanceIdentifier(aws.StringValue(input.DBInstanceIdentifier)); err != nil {
		return nil, err
	}
	if err := validateDBSnapshotReference(aws.StringValue(input.DBSnapshotIdentifier)); err != nil {
		return nil, err
	}
	if err := rejectUnimplementedRestore(input); err != nil {
		return nil, err
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	snapshot, _, err := s.getDBSnapshot(ctx, kv, aws.StringValue(input.DBSnapshotIdentifier))
	if err != nil {
		return nil, err
	}
	if snapshot.Status != SnapshotStatusAvailable || snapshot.SnapshotID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorDBSnapshotInvalidState,
			"DB snapshot %s is %s; it must be %s to restore from",
			snapshot.DBSnapshotIdentifier, snapshot.Status, SnapshotStatusAvailable)
	}

	req, err := s.resolveRestoreRequest(input, snapshot)
	if err != nil {
		return nil, err
	}
	placement, err := s.resolvePlacement(ctx, kv, accountID, req)
	if err != nil {
		return nil, err
	}
	parameters, err := s.resolveGroupParameters(ctx, kv, accountID, req.DBParameterGroupName, req.InstanceClass)
	if err != nil {
		return nil, err
	}
	// The agent cannot bootstrap without this profile, so resolve it before the
	// identifier reservation or restored-volume creation.
	profileARN, err := ensureInstanceProfile(s.deps.IAM, utils.GlobalAccountID)
	if err != nil {
		return nil, err
	}

	// The record is the identifier's reservation, exactly as at create, and is
	// withdrawn on any failure below.
	key := DBInstanceKey(req.Identifier)
	rec := newRestoredDBInstanceRecord(accountID, req, placement, parameters, snapshot)
	rollbackRev, createErr := createJSONRevision(ctx, kv, key, &rec)
	if createErr != nil {
		if errors.Is(createErr, jetstream.ErrKeyExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBInstanceAlreadyExists,
				"DB instance %s already exists", req.Identifier)
		}
		return nil, createErr
	}
	defer func() {
		if err == nil {
			return
		}
		s.rollbackDBInstanceReservation(ctx, kv, key, req.Identifier, rec.DbiResourceID, rollbackRev)
	}()

	// The datadir the restored engine comes up on. Created before the VM because
	// the launch attaches an existing volume rather than making one — and unwound
	// here, since only a launch that created a volume unwinds it.
	volume, err := s.createRestoreVolume(ctx, req, snapshot)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.discardRestoreVolume(ctx, volume.id)
		}
	}()
	// Read from the volume rather than echoed from the snapshot: a cluster whose
	// storage key has gone would otherwise report encryption it is not giving.
	if snapshot.StorageEncrypted && !volume.encrypted {
		return nil, fmt.Errorf("rds: volume %s restored from %s came back unencrypted; the cluster storage key is not configured",
			volume.id, snapshot.DBSnapshotIdentifier)
	}

	launched, err := LaunchDBInstanceVM(ctx, s.deps.Launch, LaunchInput{
		DBInstanceIdentifier: req.Identifier,
		AccountID:            accountID,
		SubnetID:             placement.SubnetID,
		SecurityGroupIDs:     placement.SecurityGroupIDs,
		Engine:               req.Engine.Name,
		EngineVersion:        req.EngineVersion,
		InstanceType:         req.InstanceType,
		AllocatedStorage:     req.AllocatedStorage,
		ExistingDataVolume:   volume.id,
		UserData: buildAgentUserData(agentUserDataInput{
			GatewayURL:           s.deps.GatewayURL,
			GatewayCACert:        s.deps.GatewayCACert,
			Region:               s.region,
			DBInstanceIdentifier: req.Identifier,
			EngineVersion:        req.EngineVersion,
			EnginePort:           req.Port,
		}),
		IamInstanceProfileArn: profileARN,
	})
	if err != nil {
		return nil, err
	}

	stored, launchRev, err := s.recordLaunch(ctx, kv, key, accountID, rec.DbiResourceID, launched)
	if err != nil {
		s.unwindLaunched(ctx, launched)
		return nil, err
	}
	rollbackRev = launchRev
	if err = s.PutInstanceIndex(ctx, launched.InstanceID, InstanceIndexEntry{
		AccountID:            accountID,
		DBInstanceIdentifier: req.Identifier,
		VMGeneration:         firstVMGeneration,
	}); err != nil {
		s.unwindLaunched(ctx, launched)
		return nil, fmt.Errorf("rds: write instance index for %s: %w", req.Identifier, err)
	}
	s.publishDNS(ctx, accountID, stored, handlers_dns.ActionUpsert)

	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, req.Identifier,
		fmt.Sprintf("Restored from DB snapshot %s.", snapshot.DBSnapshotIdentifier),
		EventCategoryCreation, EventCategoryRecovery)
	slog.InfoContext(ctx, "rds: DB instance restored from snapshot",
		"dbInstance", req.Identifier, "dbSnapshot", snapshot.DBSnapshotIdentifier,
		"accountId", accountID, "instanceId", launched.InstanceID, "dataVolumeId", volume.id)

	return &rds.RestoreDBInstanceFromDBSnapshotOutput{DBInstance: s.projectDBInstance(stored)}, nil
}

// Request-overridable configuration comes from the snapshot when omitted.
// Platform-owned settings use the defaults in force when the restore runs.
func (s *Service) resolveRestoreRequest(input *rds.RestoreDBInstanceFromDBSnapshotInput, snapshot *DBSnapshotRecord) (*validatedCreate, error) {
	// The snapshot's engine, never the request's: the datadir is written in one
	// engine's on-disk format and no other can read it.
	engine, err := LookupEngine(snapshot.Engine)
	if err != nil {
		return nil, err
	}
	if requested := aws.StringValue(input.Engine); requested != "" && !strings.EqualFold(requested, engine.Name) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DB snapshot %s holds %s data; restoring it as %s is not offered",
			snapshot.DBSnapshotIdentifier, engine.Name, requested)
	}

	instanceClass := aws.StringValue(input.DBInstanceClass)
	if instanceClass == "" {
		instanceClass = snapshot.DBInstanceClass
	}
	instanceType, err := InstanceTypeForClass(instanceClass)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBInstanceClass %q is not supported; supported classes are %s", instanceClass, strings.Join(SupportedInstanceClasses(), ", "))
	}

	storage, err := resolveRestoreStorage(input, snapshot)
	if err != nil {
		return nil, err
	}
	storageType, err := resolveRestoreStorageType(input, snapshot)
	if err != nil {
		return nil, err
	}
	port, err := resolveRestorePort(input, snapshot, engine)
	if err != nil {
		return nil, err
	}

	if name := aws.StringValue(input.DBName); name != "" && name != snapshot.DBName {
		return nil, unimplemented("DBName",
			"a restore starts on the snapshot's datadir, so it cannot create a database the snapshot does not hold")
	}
	paramGroup := aws.StringValue(input.DBParameterGroupName)
	if paramGroup == "" {
		paramGroup = snapshot.DBParameterGroupName
	}
	if paramGroup == "" {
		paramGroup = engine.DefaultParameterGroupName()
	}
	subnetGroup := aws.StringValue(input.DBSubnetGroupName)
	if subnetGroup == "" {
		subnetGroup = snapshot.DBSubnetGroupName
	}
	securityGroups := aws.StringValueSlice(input.VpcSecurityGroupIds)
	if len(securityGroups) == 0 {
		securityGroups = snapshot.VpcSecurityGroupIDs
	}

	tagMap, err := validateTags(input.Tags)
	if err != nil {
		return nil, err
	}

	return &validatedCreate{
		Identifier:       aws.StringValue(input.DBInstanceIdentifier),
		Engine:           engine,
		EngineVersion:    snapshot.EngineVersion,
		InstanceClass:    instanceClass,
		InstanceType:     instanceType,
		AllocatedStorage: storage,
		StorageType:      storageType,
		Port:             port,
		MasterUsername:   snapshot.MasterUsername,
		// Deliberately empty: the datadir already holds the master role and its
		// password hash, so there is nothing for a bootstrap to set.
		DBName:               snapshot.DBName,
		SecurityGroupIDs:     securityGroups,
		DBSubnetGroupName:    subnetGroup,
		DBParameterGroupName: paramGroup,
		DeletionProtection:   aws.BoolValue(input.DeletionProtection),

		AutoMinorVersionUpgrade: input.AutoMinorVersionUpgrade == nil || aws.BoolValue(input.AutoMinorVersionUpgrade),
		BackupRetentionPeriod:   s.defaultRetentionDays(),

		Tags: tagMap,
	}, nil
}

// Storage may only be grown: CreateVolume refuses a size below the snapshot's,
// and a shrink has nowhere to put the data the snapshot already holds.
func resolveRestoreStorage(input *rds.RestoreDBInstanceFromDBSnapshotInput, snapshot *DBSnapshotRecord) (int64, error) {
	if input.AllocatedStorage == nil {
		return snapshot.AllocatedStorage, nil
	}
	requested := aws.Int64Value(input.AllocatedStorage)
	if requested < snapshot.AllocatedStorage {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"AllocatedStorage may not be below the %d GiB DB snapshot %s holds",
			snapshot.AllocatedStorage, snapshot.DBSnapshotIdentifier)
	}
	if requested > maxAllocatedStorageGiB {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"AllocatedStorage must be between %d and %d GiB", minAllocatedStorageGiB, maxAllocatedStorageGiB)
	}
	return requested, nil
}

func resolveRestoreStorageType(input *rds.RestoreDBInstanceFromDBSnapshotInput, snapshot *DBSnapshotRecord) (string, error) {
	storageType := strings.ToLower(strings.TrimSpace(aws.StringValue(input.StorageType)))
	if storageType == "" {
		storageType = snapshot.StorageType
	}
	if storageType == "" {
		return storageTypeGP3, nil
	}
	if storageType != storageTypeGP3 {
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"StorageType %q is not supported; only %q is offered", storageType, storageTypeGP3)
	}
	return storageType, nil
}

func resolveRestorePort(input *rds.RestoreDBInstanceFromDBSnapshotInput, snapshot *DBSnapshotRecord, engine Engine) (int64, error) {
	if input.Port != nil {
		port := aws.Int64Value(input.Port)
		if port < minDBPort || port > maxDBPort {
			return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"Port must be between %d and %d", minDBPort, maxDBPort)
		}
		return port, nil
	}
	if snapshot.Port > 0 {
		return snapshot.Port, nil
	}
	return engine.DefaultPort, nil
}

// A parameter that would create a false safety, security or availability
// guarantee is rejected rather than silently dropped. The list mirrors the one
// at create, plus what only a restore can be asked for.
func rejectUnimplementedRestore(input *rds.RestoreDBInstanceFromDBSnapshotInput) error {
	if aws.BoolValue(input.MultiAZ) {
		return unimplemented("MultiAZ", "this platform is single-AZ; a standby would not exist")
	}
	if aws.BoolValue(input.PubliclyAccessible) {
		return unimplemented("PubliclyAccessible",
			"the endpoint is a private VPC address; a public one would not be reachable")
	}
	if aws.StringValue(input.DBClusterSnapshotIdentifier) != "" {
		return unimplemented("DBClusterSnapshotIdentifier", "clustered engines are not offered")
	}
	if aws.Int64Value(input.Iops) > 0 {
		return unimplemented("Iops", "provisioned IOPS are not implemented; storage is gp3")
	}
	if aws.Int64Value(input.StorageThroughput) > 0 {
		return unimplemented("StorageThroughput", "provisioned throughput is not implemented; storage is gp3")
	}
	if aws.StringValue(input.AvailabilityZone) != "" {
		return unimplemented("AvailabilityZone", "this platform exposes a single zone")
	}
	if aws.BoolValue(input.EnableIAMDatabaseAuthentication) {
		return unimplemented("EnableIAMDatabaseAuthentication", "IAM database authentication is not implemented")
	}
	if len(input.EnableCloudwatchLogsExports) > 0 {
		return unimplemented("EnableCloudwatchLogsExports", "log export is not implemented")
	}
	if aws.StringValue(input.OptionGroupName) != "" {
		return unimplemented("OptionGroupName", "option groups are not offered")
	}
	if aws.StringValue(input.Domain) != "" {
		return unimplemented("Domain", "Active Directory domain joining is not implemented")
	}
	if aws.StringValue(input.CustomIamInstanceProfile) != "" {
		return unimplemented("CustomIamInstanceProfile", "the DB VM's instance profile is platform-owned")
	}
	if aws.StringValue(input.TdeCredentialArn) != "" {
		return unimplemented("TdeCredentialArn", "storage is encrypted with the cluster key, not a TDE credential")
	}
	if aws.BoolValue(input.EnableCustomerOwnedIp) {
		return unimplemented("EnableCustomerOwnedIp", "customer-owned IP addressing is an Outposts feature")
	}
	return nil
}

// The reserved record for a restored instance: everything the request and the
// snapshot decided, with the bootstrap marked consumed so the agent's first
// fetch returns attach rather than a password it would use to run initdb.
func newRestoredDBInstanceRecord(accountID string, req *validatedCreate, placement *endpointPlacement,
	parameters []Parameter, snapshot *DBSnapshotRecord) DBInstanceRecord {
	rec := newDBInstanceRecord(accountID, req, placement, parameters)
	now := time.Now().UTC()
	rec.Bootstrap.Consumed = true
	rec.Bootstrap.ConsumedAt = &now
	// Carried over so a later ModifyDBInstance --master-user-password still reads
	// as a rotation of the credentials the datadir was written with.
	rec.MasterPasswordUpdatedAt = snapshot.MasterPasswordUpdatedAt
	rec.StorageEncrypted = snapshot.StorageEncrypted
	rec.RestoredFromDBSnapshot = snapshot.DBSnapshotIdentifier
	return rec
}

// The restored data volume and what it reported about itself.
type restoredVolume struct {
	id        string
	encrypted bool
}

// System-account owned like every RDS data volume, so the customer reaches it
// only through the DB instance.
func (s *Service) createRestoreVolume(ctx context.Context, req *validatedCreate, snapshot *DBSnapshotRecord) (*restoredVolume, error) {
	if s.deps.Launch.Volume == nil || s.deps.Launch.Config == nil {
		return nil, errors.New("rds: no volume service configured")
	}
	volume, err := s.deps.Launch.Volume.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(s.deps.Launch.Config.AZ),
		Size:             aws.Int64(req.AllocatedStorage),
		VolumeType:       aws.String(storageTypeGP3),
		SnapshotId:       aws.String(snapshot.SnapshotID),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("volume"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByRDS)},
				{Key: aws.String(rdsInstanceTagKey), Value: aws.String(req.Identifier)},
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		return nil, fmt.Errorf("rds: create the data volume for %s from %s: %w",
			req.Identifier, snapshot.DBSnapshotIdentifier, err)
	}
	if volume == nil || aws.StringValue(volume.VolumeId) == "" {
		return nil, fmt.Errorf("rds: create the data volume for %s from %s: empty volume id",
			req.Identifier, snapshot.DBSnapshotIdentifier)
	}
	return &restoredVolume{id: aws.StringValue(volume.VolumeId), encrypted: aws.BoolValue(volume.Encrypted)}, nil
}

// A volume left behind would hold its source snapshot undeletable for a restore
// that never produced an instance, so the delete runs on a context detached from
// the caller's.
func (s *Service) discardRestoreVolume(ctx context.Context, volumeID string) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if _, err := s.deps.Launch.Volume.DeleteVolume(rbCtx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, utils.GlobalAccountID); err != nil && !awserrors.IsNotFound(err) {
		slog.WarnContext(rbCtx, "rds: rollback delete of a restored data volume failed",
			"volumeId", volumeID, "err", err)
	}
}
