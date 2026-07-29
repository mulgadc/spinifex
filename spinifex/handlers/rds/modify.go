package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// AWS's bound on automated-backup retention. Nothing consumes it until rds-9,
// but accepting a value outside the range would store a setting that fails
// later, in a window nobody is watching.
const maxBackupRetentionDays = 35

// The resolved effect of a ModifyDBInstance request: what changes now and what
// is left for a maintenance window. Every field is a *difference* from the
// stored record — Terraform sends the whole body on every apply, so a field
// repeating its current value contributes nothing rather than an outage.
type modifyPlan struct {
	// Applied on the spot whatever ApplyImmediately says, because none of them
	// interrupts service and AWS applies them as soon as possible too. The
	// password especially: D8 forbids persisting the cleartext, so there is
	// nothing that could be deferred.
	MasterUserPassword         string
	SecurityGroupIDs           []string
	DeletionProtection         *bool
	BackupRetentionPeriod      *int64
	PreferredBackupWindow      string
	PreferredMaintenanceWindow string

	// Disruptive: each one takes the engine down, so ApplyImmediately=false
	// records them as pending instead of applying them.
	AllocatedStorage *int64
	InstanceClass    string
	InstanceType     string
	ParameterGroup   string

	ApplyImmediately bool
}

// Whether anything in the plan takes the engine down, which is what decides
// between applying now and recording pending values.
func (p *modifyPlan) disruptive() bool {
	return p.AllocatedStorage != nil || p.InstanceClass != "" || p.ParameterGroup != ""
}

// Whether anything lands without an outage, which is also what decides whether
// the record write and its event happen at all: a modify that is entirely
// deferred must not report a configuration change that has not happened.
func (p *modifyPlan) immediate() bool {
	return p.MasterUserPassword != "" || p.SecurityGroupIDs != nil || p.DeletionProtection != nil ||
		p.BackupRetentionPeriod != nil || p.PreferredBackupWindow != "" || p.PreferredMaintenanceWindow != ""
}

func (p *modifyPlan) empty() bool {
	return !p.disruptive() && !p.immediate()
}

// Changes a live DB instance. The non-disruptive settings land immediately; a
// storage grow, a class change or a parameter-group change is applied now when
// ApplyImmediately is set and otherwise recorded in PendingModifiedValues for
// the maintenance window (rds-9) to drain through applyPendingModifications.
//
// The endpoint survives every path: the data volume, the customer ENI and its
// address, and the DNS A-record are untouched by a grow and by a class change.
func (s *Service) ModifyDBInstance(ctx context.Context, input *rds.ModifyDBInstanceInput, accountID string) (*rds.ModifyDBInstanceOutput, error) {
	if input == nil {
		return nil, fmt.Errorf("%s: empty request", awserrors.ErrorInvalidParameterValue)
	}
	id := aws.StringValue(input.DBInstanceIdentifier)
	if id == "" {
		return nil, fmt.Errorf("%s: DBInstanceIdentifier is required", awserrors.ErrorInvalidParameterValue)
	}
	if err := rejectUnimplementedModify(input); err != nil {
		return nil, err
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, _, err := s.getDBInstance(ctx, kv, id)
	if err != nil {
		return nil, err
	}

	// Resolved against the stored record and fully validated before anything
	// moves, so a rejected request never leaves a database stopped.
	plan, err := s.planModify(ctx, input, accountID, rec)
	if err != nil {
		return nil, err
	}
	if plan.empty() {
		return &rds.ModifyDBInstanceOutput{DBInstance: s.projectDBInstance(rec)}, nil
	}
	// A disruptive change needs a live engine to be stopped cleanly and a live
	// agent to apply parameters, so it is legal only from available. The
	// non-disruptive settings are record and ENI writes, legal from any settled
	// state.
	if plan.disruptive() && rec.Status != StatusAvailable && rec.Status != StatusFailed {
		return nil, fmt.Errorf("%s: DB instance %s is %s; the requested modification requires it to be %s",
			awserrors.ErrorDBInstanceInvalidState, id, rec.Status, StatusAvailable)
	}

	if err := s.applyImmediateModify(ctx, kv, accountID, rec, plan); err != nil {
		return nil, err
	}
	if !plan.disruptive() {
		stored, err := s.reloadInstance(ctx, kv, id)
		if err != nil {
			return nil, err
		}
		return &rds.ModifyDBInstanceOutput{DBInstance: s.projectDBInstance(stored)}, nil
	}

	pending := &PendingModifiedValues{
		AllocatedStorage:     plan.AllocatedStorage,
		DBInstanceClass:      plan.InstanceClass,
		DBParameterGroupName: plan.ParameterGroup,
		RequestedAt:          time.Now().UTC(),
	}
	// Recorded before any of it is attempted, so a leader that dies part-way
	// leaves the next one enough to finish rather than an instance stuck in
	// modifying with no record of what it was becoming.
	if err := s.updateInstance(ctx, kv, id, func(stored *DBInstanceRecord) {
		stored.PendingModifiedValues = pending
	}); err != nil {
		return nil, err
	}
	rec.PendingModifiedValues = pending

	if !plan.ApplyImmediately {
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, id,
			"Modification recorded; it will be applied during the next maintenance window, and the database will be unavailable while it is.",
			EventCategoryConfigurationChange, EventCategoryNotification)
		return &rds.ModifyDBInstanceOutput{DBInstance: s.projectDBInstance(rec)}, nil
	}

	moved, kv, err := s.beginTransition(ctx, accountID, id, StatusModifying, StatusAvailable, StatusFailed)
	if err != nil {
		return nil, err
	}
	// Under the lease, or the reconciler's sweep of modifying instances re-enters
	// this same change while it is still running.
	if _, err := s.withModifyLease(ctx, kv, id, func() error {
		return s.applyPendingModifications(ctx, kv, accountID, moved)
	}); err != nil {
		return nil, s.failTransition(ctx, kv, accountID, moved,
			fmt.Sprintf("the DB instance could not be modified: %v", err))
	}

	// Returned as modifying: the replacement or restarted VM has to come back
	// and report healthy before the reconciler calls it available.
	stored, err := s.reloadInstance(ctx, kv, id)
	if err != nil {
		return nil, err
	}
	return &rds.ModifyDBInstanceOutput{DBInstance: s.projectDBInstance(stored)}, nil
}

// The record as stored, for a response that reports what actually landed rather
// than what the handler believed it wrote.
func (s *Service) reloadInstance(ctx context.Context, kv jetstream.KeyValue, id string) (*DBInstanceRecord, error) {
	rec, _, err := s.getDBInstance(ctx, kv, id)
	return rec, err
}

// Resolves the request against the stored record: drops every field that
// repeats its current value, and rejects everything that cannot be delivered.
func (s *Service) planModify(ctx context.Context, input *rds.ModifyDBInstanceInput, accountID string, rec *DBInstanceRecord) (*modifyPlan, error) {
	plan := &modifyPlan{ApplyImmediately: aws.BoolValue(input.ApplyImmediately)}

	if password := aws.StringValue(input.MasterUserPassword); password != "" {
		if err := ValidateMasterUserPassword(password); err != nil {
			return nil, err
		}
		plan.MasterUserPassword = password
	}

	if storage := aws.Int64Value(input.AllocatedStorage); storage > 0 && storage != rec.AllocatedStorage {
		if err := validateStorageGrow(rec.AllocatedStorage, storage); err != nil {
			return nil, err
		}
		plan.AllocatedStorage = aws.Int64(storage)
	}

	if class := aws.StringValue(input.DBInstanceClass); class != "" && class != rec.DBInstanceClass {
		instanceType, err := InstanceTypeForClass(class)
		if err != nil {
			return nil, fmt.Errorf("%s: DBInstanceClass %q is not supported; supported classes are %s",
				awserrors.ErrorInvalidParameterValue, class, strings.Join(SupportedInstanceClasses(), ", "))
		}
		plan.InstanceClass, plan.InstanceType = class, instanceType
	}

	// Resolved against KV here rather than at apply time, so a group that does
	// not exist is rejected before the instance is moved into modifying.
	if group := aws.StringValue(input.DBParameterGroupName); group != "" && group != rec.DBParameterGroupName {
		kv, err := s.bucket(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if _, _, err := getDBParameterGroup(ctx, kv, accountID, group); err != nil {
			return nil, err
		}
		plan.ParameterGroup = group
	}

	groups, err := s.planSecurityGroups(ctx, accountID, rec, aws.StringValueSlice(input.VpcSecurityGroupIds))
	if err != nil {
		return nil, err
	}
	plan.SecurityGroupIDs = groups

	if input.DeletionProtection != nil && aws.BoolValue(input.DeletionProtection) != rec.DeletionProtection {
		plan.DeletionProtection = input.DeletionProtection
	}
	if err := planBackupSettings(input, rec, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// The security groups to re-associate, or nil when the request names none or
// names the set already attached. Validated against the endpoint ENI's own VPC,
// because an ENI cannot carry a group from another one.
func (s *Service) planSecurityGroups(ctx context.Context, accountID string, rec *DBInstanceRecord, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	if slices.Equal(requested, rec.VpcSecurityGroupIDs) {
		return nil, nil
	}
	if rec.ENIID == "" {
		return nil, fmt.Errorf("%s: DB instance %s has no endpoint ENI to re-associate",
			awserrors.ErrorDBInstanceInvalidState, rec.DBInstanceIdentifier)
	}
	// ModifyNetworkInterfaceAttribute validates the groups against the ENI's VPC
	// itself, but doing it here keeps the rejection ahead of every other write in
	// the modify rather than half-way through it.
	if _, err := s.resolveSecurityGroups(ctx, accountID, rec.VpcID, requested); err != nil {
		return nil, err
	}
	return requested, nil
}

// The rds-9 fields. Stored rather than acted on here, but still bounds-checked:
// a value outside AWS's range would fail in a maintenance window nobody is
// watching rather than in the call that set it.
func planBackupSettings(input *rds.ModifyDBInstanceInput, rec *DBInstanceRecord, plan *modifyPlan) error {
	if input.BackupRetentionPeriod != nil {
		days := aws.Int64Value(input.BackupRetentionPeriod)
		if days < 0 || days > maxBackupRetentionDays {
			return fmt.Errorf("%s: BackupRetentionPeriod must be between 0 and %d days",
				awserrors.ErrorInvalidParameterValue, maxBackupRetentionDays)
		}
		if days != rec.BackupRetentionPeriod {
			plan.BackupRetentionPeriod = aws.Int64(days)
		}
	}
	if window := aws.StringValue(input.PreferredBackupWindow); window != "" && window != rec.PreferredBackupWindow {
		plan.PreferredBackupWindow = window
	}
	if window := aws.StringValue(input.PreferredMaintenanceWindow); window != "" && window != rec.PreferredMaintenanceWindow {
		plan.PreferredMaintenanceWindow = window
	}
	return nil
}

// The half of the plan that never takes the engine down: a live password
// rotation, an ENI security-group re-association, and record settings. Each is
// applied before any disruptive work, so a modify that carries both does not
// lose the cheap half to a failure in the expensive one.
func (s *Service) applyImmediateModify(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord, plan *modifyPlan) error {
	if !plan.immediate() {
		return nil
	}
	// Never persisted anywhere: an unreachable agent fails the call rather than
	// leaving cleartext queued for a later window (D8).
	if plan.MasterUserPassword != "" {
		if err := s.setMasterPassword(ctx, accountID, rec.DBInstanceIdentifier, rec.MasterUsername, plan.MasterUserPassword); err != nil {
			return fmt.Errorf("%s: the master password could not be applied: %w", awserrors.ErrorDBInstanceInvalidState, err)
		}
	}
	// Done before the record write so a rejected group leaves the record still
	// naming the groups the ENI actually carries.
	if len(plan.SecurityGroupIDs) > 0 {
		if err := s.reassociateSecurityGroups(ctx, accountID, rec, plan.SecurityGroupIDs); err != nil {
			return err
		}
	}

	now := time.Now().UTC()
	if err := s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		if plan.MasterUserPassword != "" {
			stored.MasterPasswordUpdatedAt = &now
		}
		if len(plan.SecurityGroupIDs) > 0 {
			stored.VpcSecurityGroupIDs = plan.SecurityGroupIDs
		}
		if plan.DeletionProtection != nil {
			stored.DeletionProtection = *plan.DeletionProtection
		}
		if plan.BackupRetentionPeriod != nil {
			stored.BackupRetentionPeriod = *plan.BackupRetentionPeriod
		}
		if plan.PreferredBackupWindow != "" {
			stored.PreferredBackupWindow = plan.PreferredBackupWindow
		}
		if plan.PreferredMaintenanceWindow != "" {
			stored.PreferredMaintenanceWindow = plan.PreferredMaintenanceWindow
		}
	}); err != nil {
		return err
	}

	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"DB instance configuration changed.", EventCategoryConfigurationChange)
	return nil
}

// Changing a database's ingress is routine day-two work, and the ENI already
// owns the whole job: the groups are validated against the account and the
// ENI's VPC and the OVN binding is republished. No VM replace, no new address,
// so the endpoint the customer connects to does not move.
func (s *Service) reassociateSecurityGroups(ctx context.Context, accountID string, rec *DBInstanceRecord, groups []string) error {
	if s.deps.Launch.VPC == nil {
		return errors.New("rds: no VPC path configured")
	}
	if _, err := s.deps.Launch.VPC.ModifyNetworkInterfaceAttribute(ctx, &ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(rec.ENIID),
		Groups:             aws.StringSlice(groups),
	}, accountID); err != nil {
		return fmt.Errorf("rds: re-associate the security groups of %s: %w", rec.DBInstanceIdentifier, err)
	}
	slog.InfoContext(ctx, "rds: endpoint security groups re-associated",
		"dbInstance", rec.DBInstanceIdentifier, "eniId", rec.ENIID, "groups", groups)
	return nil
}

// D19: a supported action carrying an unimplemented parameter must not silently
// drop it. Every rejection below is a parameter whose omission would create a
// false safety, security or availability guarantee; the inert ones —
// AutoMinorVersionUpgrade, CopyTagsToSnapshot, Performance Insights, Enhanced
// Monitoring — are deliberately absent and accepted as no-ops.
func rejectUnimplementedModify(input *rds.ModifyDBInstanceInput) error {
	if aws.BoolValue(input.MultiAZ) {
		return unimplemented("MultiAZ", "this platform is single-AZ; a standby would not exist")
	}
	if aws.BoolValue(input.PubliclyAccessible) {
		return unimplemented("PubliclyAccessible",
			"the endpoint is a private VPC address; a public one would not be reachable")
	}
	if aws.StringValue(input.NewDBInstanceIdentifier) != "" {
		return unimplemented("NewDBInstanceIdentifier",
			"the identifier is the endpoint hostname and the certificate's subject; renaming in place is not implemented")
	}
	if aws.StringValue(input.EngineVersion) != "" {
		return unimplemented("EngineVersion", "engine-version upgrade is not implemented")
	}
	if aws.StringValue(input.Engine) != "" {
		return unimplemented("Engine", "an instance cannot change engine")
	}
	if aws.Int64Value(input.DBPortNumber) > 0 {
		return unimplemented("DBPortNumber",
			"the port is fixed at create; changing it would break every client and the serving certificate")
	}
	if aws.StringValue(input.DBSubnetGroupName) != "" {
		return unimplemented("DBSubnetGroupName",
			"the endpoint ENI is placed at create and moving it would change the address clients resolve")
	}
	if aws.Int64Value(input.MaxAllocatedStorage) > 0 {
		return unimplemented("MaxAllocatedStorage", "storage autoscaling is not implemented")
	}
	if aws.Int64Value(input.Iops) > 0 {
		return unimplemented("Iops", "provisioned IOPS are not implemented; storage is gp3")
	}
	if aws.Int64Value(input.StorageThroughput) > 0 {
		return unimplemented("StorageThroughput", "provisioned throughput is not implemented; storage is gp3")
	}
	if storageType := aws.StringValue(input.StorageType); storageType != "" && strings.ToLower(storageType) != storageTypeGP3 {
		return unimplemented("StorageType", "only gp3 is offered, so no other type can be moved to")
	}
	if aws.BoolValue(input.ManageMasterUserPassword) || aws.BoolValue(input.RotateMasterUserPassword) {
		return unimplemented("ManageMasterUserPassword",
			"there is no Secrets Manager integration; supply MasterUserPassword instead")
	}
	if aws.BoolValue(input.EnableIAMDatabaseAuthentication) {
		return unimplemented("EnableIAMDatabaseAuthentication", "IAM database authentication is not implemented")
	}
	if len(input.DBSecurityGroups) > 0 {
		return unimplemented("DBSecurityGroups",
			"EC2-Classic security groups are not offered; use VpcSecurityGroupIds")
	}
	if aws.StringValue(input.CACertificateIdentifier) != "" {
		return unimplemented("CACertificateIdentifier",
			"the serving certificate is signed by the cluster CA, which is not selectable")
	}
	if aws.StringValue(input.Domain) != "" || aws.StringValue(input.DomainFqdn) != "" {
		return unimplemented("Domain", "Active Directory domain join is not offered")
	}
	if aws.StringValue(input.OptionGroupName) != "" {
		return unimplemented("OptionGroupName", "option groups are not offered")
	}
	return nil
}
