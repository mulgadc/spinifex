package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// The first VM behind a DB instance. Replacement and recovery increment it,
// so an agent report carrying an older generation is a superseded VM.
const firstVMGeneration = 1

// Bounds retries when same-owner record writes race rollback cleanup.
const rollbackDeleteAttempts = 3

// Assembles a live DB instance out of the launch primitives: validate, reserve
// the identifier, place and launch the dual-NIC VM with its data volume and
// customer ENI, seed the one-shot bootstrap config, and publish the endpoint
// record. The instance is returned at status=creating; the reconciler flips it
// to available on the first healthy agent heartbeat.
func (s *Service) CreateDBInstance(ctx context.Context, input *rds.CreateDBInstanceInput, accountID string) (out *rds.CreateDBInstanceOutput, err error) {
	req, err := s.validateCreateRequest(input)
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	placement, err := s.resolvePlacement(ctx, kv, accountID, req)
	if err != nil {
		return nil, err
	}
	// Resolved before the identifier is reserved, so a create naming a group that
	// does not exist leaves no record behind. The set is literals only: the
	// class's memory has already been folded into every size-derived default, so
	// the agent never sees a formula.
	parameters, err := s.resolveGroupParameters(ctx, kv, accountID, req.Engine, req.DBParameterGroupName, req.InstanceClass)
	if err != nil {
		return nil, err
	}
	// The agent cannot bootstrap without this profile, so resolve it before the
	// identifier reservation or any launch side effects.
	profileARN, err := ensureInstanceProfile(s.deps.IAM, utils.GlobalAccountID)
	if err != nil {
		return nil, err
	}
	key := DBInstanceKey(req.Identifier)

	// Creating the record before anything is provisioned makes the identifier
	// uniqueness check atomic against a concurrent create on another node — a
	// prior read-then-write would let both pass and both launch a VM.
	rec := newDBInstanceRecord(accountID, req, placement, parameters)
	rollbackRev, createErr := createJSONRevision(ctx, kv, key, &rec)
	if createErr != nil {
		if errors.Is(createErr, jetstream.ErrKeyExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBInstanceAlreadyExists,
				"DB instance %s already exists", req.Identifier)
		}
		return nil, createErr
	}

	// The record is the reservation, so it is withdrawn on any failure below.
	// The revision guard leaves a concurrent recreation of the identifier intact.
	defer func() {
		if err == nil {
			return
		}
		s.rollbackDBInstanceReservation(ctx, kv, key, req.Identifier, rec.DbiResourceID, rollbackRev)
	}()

	// Staged before the launch so no VM can boot ahead of the password it needs,
	// and bound to the first generation the record will carry. The rollback above
	// removes it with the reservation.
	if _, err = s.writeBootstrapPayload(ctx, kv, accountID, &rec, req.MasterPassword); err != nil {
		return nil, err
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
		UserData: buildAgentUserData(agentUserDataInput{
			GatewayURL:           s.deps.GatewayURL,
			GatewayCACert:        s.deps.GatewayCACert,
			Region:               s.region,
			DBInstanceIdentifier: req.Identifier,
			Engine:               req.Engine.Name,
			EngineVersion:        req.EngineVersion,
			EnginePort:           req.Port,
		}),
		// The system account owns the VM, so the role is ensured there too — the
		// gateway's agent gate requires an assumed-role session in that account.
		IamInstanceProfileArn: profileARN,
	})
	if err != nil {
		return nil, err
	}

	// The launch already unwound everything on failure, so from here the resources
	// exist and the record has to catch up with them.
	stored, launchRev, err := s.recordLaunch(ctx, kv, key, accountID, rec.DbiResourceID, launched, true)
	if err != nil {
		s.unwindLaunched(ctx, launched)
		return nil, err
	}
	rollbackRev = launchRev

	// Written before the DNS publish because an agent that boots fast enough to
	// call the gateway before this exists is denied and has to retry.
	if err = s.PutInstanceIndex(ctx, launched.InstanceID, InstanceIndexEntry{
		AccountID:            accountID,
		DBInstanceIdentifier: req.Identifier,
		VMGeneration:         firstVMGeneration,
	}); err != nil {
		s.unwindLaunched(ctx, launched)
		return nil, fmt.Errorf("rds: write instance index for %s: %w", req.Identifier, err)
	}

	// Published as soon as the ENI IP is known rather than on available, so the
	// name resolves the moment the engine comes up; the security group is what
	// gates connectivity until then.

	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, req.Identifier,
		"DB instance created.", EventCategoryCreation)
	slog.InfoContext(ctx, "rds: DB instance created",
		"dbInstance", req.Identifier, "accountId", accountID, "instanceId", launched.InstanceID,
		"endpoint", stored.EndpointAddress, "class", req.InstanceClass)

	return &rds.CreateDBInstanceOutput{DBInstance: s.projectDBInstance(stored)}, nil
}

// The record as it stands before any resource exists: everything the request
// determined. The master password is never on the record — it is staged
// encrypted under its own key, bound to the generation set here.
func newDBInstanceRecord(accountID string, req *validatedCreate, placement *endpointPlacement, parameters []Parameter) DBInstanceRecord {
	now := time.Now().UTC()
	return DBInstanceRecord{
		DBInstanceIdentifier: req.Identifier,
		DbiResourceID:        utils.GenerateResourceID(dbiResourceIDPrefix),
		AccountID:            accountID,
		Status:               StatusCreating,
		VMGeneration:         firstVMGeneration,
		Engine:               req.Engine.Name,
		EngineVersion:        req.EngineVersion,
		DBInstanceClass:      req.InstanceClass,
		AllocatedStorage:     req.AllocatedStorage,
		StorageType:          req.StorageType,
		DBName:               req.DBName,
		MasterUsername:       req.MasterUsername,
		Port:                 req.Port,
		SubnetID:             placement.SubnetID,
		VpcID:                placement.VpcID,
		VpcCIDR:              placement.VpcCIDR,
		VpcSecurityGroupIDs:  placement.SecurityGroupIDs,
		DBSubnetGroupName:    req.DBSubnetGroupName,
		DBParameterGroupName: req.DBParameterGroupName,
		DeletionProtection:   req.DeletionProtection,

		AutoMinorVersionUpgrade:   req.AutoMinorVersionUpgrade,
		CopyTagsToSnapshot:        req.CopyTagsToSnapshot,
		MonitoringInterval:        req.MonitoringInterval,
		EnablePerformanceInsights: req.EnablePerformanceInsights,

		BackupRetentionPeriod:      req.BackupRetentionPeriod,
		PreferredBackupWindow:      req.PreferredBackupWindow,
		PreferredMaintenanceWindow: req.PreferredMaintenanceWindow,

		Tags: req.Tags,
		Bootstrap: BootstrapState{
			State:              BootstrapStatePending,
			ResolvedParameters: parameters,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Folds the launch results into the reserved record and returns it as written.
// The endpoint is settled here because the ENI IP — and so the vanity name — is
// only known now.
func (s *Service) recordLaunch(ctx context.Context, kv jetstream.KeyValue, key, accountID,
	expectedResourceID string, launched *LaunchOutput, initialCreate bool) (*DBInstanceRecord, uint64, error) {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &rec)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, errors.New(awserrors.ErrorDBInstanceNotFound)
	}
	if rec.DbiResourceID != expectedResourceID {
		return nil, 0, fmt.Errorf("rds: DB instance reservation %s changed ownership during launch", key)
	}
	if launched.DataVolumeID == "" || launched.DataVolumeSerial == "" ||
		launched.DataVolumeSerial != vm.VolumeSerial(launched.DataVolumeID) {
		return nil, 0, fmt.Errorf("rds: DB instance launch %s returned invalid data volume identity", key)
	}

	rec.InstanceID = launched.InstanceID
	rec.VMGeneration = firstVMGeneration
	rec.SystemENIID = launched.SystemENIID
	rec.SystemSGID = launched.SystemSGID
	rec.ENIID = launched.CustomerENIID
	rec.ENIPrivateIP = launched.CustomerENIIP
	rec.DataVolumeID = launched.DataVolumeID
	rec.DataVolumeSerial = launched.DataVolumeSerial
	// Formatting is a create-only grant bound to this launch's exact fresh
	// volume and first VM generation. Restore and every existing-volume launch
	// pass false and cannot infer permission from an empty filesystem.
	rec.FormatAuthorized = initialCreate && launched.CreatedDataVolume && rec.VMGeneration == firstVMGeneration
	// Only a launch that made the volume can report its encryption; a restore
	// attaches one it created itself, whose encryption the record already carries.
	if launched.CreatedDataVolume {
		rec.StorageEncrypted = launched.DataVolumeEncrypted
	}
	rec.DNSName = s.dnsName(accountID, rec.DBInstanceIdentifier)
	// Without northstar there is no resolvable name, so the endpoint is the ENI
	// IP itself — stable across VM replacement and therefore as durable as a hostname.
	rec.EndpointAddress = rec.DNSName
	if rec.EndpointAddress == "" {
		rec.EndpointAddress = rec.ENIPrivateIP
	}
	rec.UpdatedAt = time.Now().UTC()

	updatedRev, err := updateJSONRevision(ctx, kv, key, rev, &rec)
	if err != nil {
		return nil, 0, err
	}
	return &rec, updatedRev, nil
}

func (s *Service) rollbackDBInstanceReservation(ctx context.Context, kv jetstream.KeyValue,
	key, identifier, resourceID string, rev uint64) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	// Before the record, so a failure below cannot leave ciphertext behind with
	// nothing naming it. Retried like the record delete: a payload left staged is
	// ciphertext at rest that nothing else reclaims.
	var payloadErr error
	for range rollbackDeleteAttempts {
		if payloadErr = deleteBootstrapPayload(rbCtx, kv, identifier); payloadErr == nil {
			break
		}
	}
	if payloadErr != nil {
		slog.WarnContext(rbCtx, "rds: rollback delete of the staged bootstrap payload failed",
			"dbInstance", identifier, "err", payloadErr)
	}

	var rollbackErr error
	for range rollbackDeleteAttempts {
		rollbackErr = kv.Delete(rbCtx, key, jetstream.LastRevision(rev))
		if rollbackErr == nil || errors.Is(rollbackErr, jetstream.ErrKeyNotFound) {
			return
		}
		if !errors.Is(rollbackErr, jetstream.ErrKeyRevisionMismatch) {
			break
		}

		// A same-owner update remains part of this failed create and must be
		// withdrawn. A different resource ID is a replacement to preserve.
		var current DBInstanceRecord
		currentRev, found, getErr := getJSONRevision(rbCtx, kv, key, &current)
		if getErr != nil {
			rollbackErr = getErr
			break
		}
		if !found || current.DbiResourceID != resourceID {
			return
		}
		rev = currentRev
	}

	slog.WarnContext(rbCtx, "rds: rollback delete of reserved DB instance record failed",
		"dbInstance", identifier, "err", rollbackErr)
}

// The launch's own rollback only covers failures inside it. A failure after it
// returned has to run the same unwind: the deferred record delete removes the
// only thing naming the VM, its two ENIs and the data volume, and nothing in
// this phase reclaims them afterwards.
func (s *Service) unwindLaunched(ctx context.Context, launched *LaunchOutput) {
	if launched == nil || launched.Unwind == nil {
		return
	}
	slog.WarnContext(ctx, "rds: unwinding a DB instance that failed after launch",
		"instanceId", launched.InstanceID, "dataVolumeId", launched.DataVolumeID,
		"customerEniId", launched.CustomerENIID, "systemEniId", launched.SystemENIID)
	launched.Unwind(ctx)
}
