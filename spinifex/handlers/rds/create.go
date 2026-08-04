package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// The first VM behind a DB instance. Replacement and recovery increment it,
// so an agent report carrying an older generation is a superseded VM.
const firstVMGeneration = 1

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
	parameters, err := s.resolveGroupParameters(ctx, kv, accountID, req.DBParameterGroupName, req.InstanceClass)
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
	if createErr := createJSON(ctx, kv, key, &rec); createErr != nil {
		if errors.Is(createErr, jetstream.ErrKeyExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBInstanceAlreadyExists,
				"DB instance %s already exists", req.Identifier)
		}
		return nil, createErr
	}

	// The record is the reservation, so it is withdrawn on any failure below:
	// leaving it behind would make the identifier permanently unusable while
	// naming an instance that was never provisioned.
	defer func() {
		if err == nil {
			return
		}
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if delErr := kv.Delete(rbCtx, key); delErr != nil {
			slog.WarnContext(rbCtx, "rds: rollback delete of reserved DB instance record failed",
				"dbInstance", req.Identifier, "err", delErr)
		}
	}()

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
	stored, err := s.recordLaunch(ctx, kv, key, accountID, launched)
	if err != nil {
		s.unwindLaunched(ctx, launched)
		return nil, err
	}

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
	s.publishDNS(ctx, accountID, stored, handlers_dns.ActionUpsert)

	slog.InfoContext(ctx, "rds: DB instance created",
		"dbInstance", req.Identifier, "accountId", accountID, "instanceId", launched.InstanceID,
		"endpoint", stored.EndpointAddress, "class", req.InstanceClass)

	return &rds.CreateDBInstanceOutput{DBInstance: s.projectDBInstance(stored)}, nil
}

// The record as it stands before any resource exists: everything the request
// determined, plus the staged master password the first bootstrap fetch consumes.
func newDBInstanceRecord(accountID string, req *validatedCreate, placement *endpointPlacement, parameters []Parameter) DBInstanceRecord {
	now := time.Now().UTC()
	return DBInstanceRecord{
		DBInstanceIdentifier: req.Identifier,
		DbiResourceID:        utils.GenerateResourceID(dbiResourceIDPrefix),
		AccountID:            accountID,
		Status:               StatusCreating,
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
		VpcSecurityGroupIDs:  placement.SecurityGroupIDs,
		DBSubnetGroupName:    req.DBSubnetGroupName,
		DBParameterGroupName: req.DBParameterGroupName,
		DeletionProtection:   req.DeletionProtection,

		AutoMinorVersionUpgrade: req.AutoMinorVersionUpgrade,

		BackupRetentionPeriod:      req.BackupRetentionPeriod,
		PreferredBackupWindow:      req.PreferredBackupWindow,
		PreferredMaintenanceWindow: req.PreferredMaintenanceWindow,

		Tags: req.Tags,
		Bootstrap: BootstrapState{
			MasterUserPassword: req.MasterPassword,
			ResolvedParameters: parameters,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Folds the launch results into the reserved record and returns it as written.
// The endpoint is settled here because the ENI IP — and so the vanity name — is
// only known now.
func (s *Service) recordLaunch(ctx context.Context, kv jetstream.KeyValue, key, accountID string, launched *LaunchOutput) (*DBInstanceRecord, error) {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorDBInstanceNotFound)
	}

	rec.InstanceID = launched.InstanceID
	rec.VMGeneration = firstVMGeneration
	rec.SystemENIID = launched.SystemENIID
	rec.ENIID = launched.CustomerENIID
	rec.ENIPrivateIP = launched.CustomerENIIP
	rec.DataVolumeID = launched.DataVolumeID
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

	if err := updateJSON(ctx, kv, key, rev, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
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
