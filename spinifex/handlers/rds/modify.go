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

// The resolved effect of a ModifyDBInstance request: what changes now and what
// is left for a maintenance window. Every field is a *difference* from the
// stored record — Terraform sends the whole body on every apply, so a field
// repeating its current value contributes nothing rather than an outage.
type modifyPlan struct {
	// Applied on the spot whatever ApplyImmediately says, because none of them
	// interrupts service and AWS applies them as soon as possible too. The
	// password especially: cleartext is never persisted, so there is
	// nothing that could be deferred.
	MasterUserPassword         string
	SecurityGroupIDs           []string
	DeletionProtection         *bool
	AutoMinorVersionUpgrade    *bool
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
		p.AutoMinorVersionUpgrade != nil || p.BackupRetentionPeriod != nil ||
		p.PreferredBackupWindow != "" || p.PreferredMaintenanceWindow != ""
}

func (p *modifyPlan) empty() bool {
	return !p.disruptive() && !p.immediate()
}

// Changes a live DB instance. The non-disruptive settings land immediately; a
// storage grow, a class change or a parameter-group change is applied now when
// ApplyImmediately is set and otherwise recorded in PendingModifiedValues for
// the maintenance window to drain through applyPendingModifications.
//
// The endpoint survives every path: the data volume, the customer ENI and its
// address, and the DNS A-record are untouched by a grow and by a class change.
func (s *Service) ModifyDBInstance(ctx context.Context, input *rds.ModifyDBInstanceInput, accountID string) (*rds.ModifyDBInstanceOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	id := aws.StringValue(input.DBInstanceIdentifier)
	if id == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBInstanceIdentifier is required")
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
		return nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
			"DB instance %s is %s; the requested modification requires it to be %s", id, rec.Status, StatusAvailable)
	}

	if err := s.applyImmediateModify(ctx, kv, accountID, rec, plan); err != nil {
		return nil, err
	}
	if !plan.disruptive() {
		stored, _, err := s.getDBInstance(ctx, kv, id)
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
	if _, err := s.withModifyLease(ctx, kv, id, func(applyCtx context.Context) error {
		return s.applyPendingModifications(applyCtx, kv, accountID, moved)
	}); err != nil {
		// Lease loss transfers recovery to another holder or the next reconcile
		// pass; the losing worker must not fail their still-retryable transition.
		if errors.Is(err, errModifyLeaseLost) {
			return nil, err
		}
		return nil, s.failTransition(ctx, kv, accountID, moved,
			fmt.Sprintf("the DB instance could not be modified: %v", err))
	}

	// Returned as modifying: the replacement or restarted VM has to come back
	// and report healthy before the reconciler calls it available.
	stored, _, err := s.getDBInstance(ctx, kv, id)
	if err != nil {
		return nil, err
	}
	return &rds.ModifyDBInstanceOutput{DBInstance: s.projectDBInstance(stored)}, nil
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
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"DBInstanceClass %q is not supported; supported classes are %s", class, strings.Join(SupportedInstanceClasses(), ", "))
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
	// Nothing acts on it, but a value the record does not adopt is one the
	// next describe contradicts, and the client re-sends it forever.
	if input.AutoMinorVersionUpgrade != nil && aws.BoolValue(input.AutoMinorVersionUpgrade) != rec.AutoMinorVersionUpgrade {
		plan.AutoMinorVersionUpgrade = input.AutoMinorVersionUpgrade
	}
	if err := s.planBackupSettings(input, rec, plan); err != nil {
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
		return nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
			"DB instance %s has no endpoint ENI to re-associate", rec.DBInstanceIdentifier)
	}
	// ModifyNetworkInterfaceAttribute validates the groups against the ENI's VPC
	// itself, but doing it here keeps the rejection ahead of every other write in
	// the modify rather than half-way through it.
	if _, err := s.resolveSecurityGroups(ctx, accountID, rec.VpcID, requested); err != nil {
		return nil, err
	}
	return requested, nil
}

// The backup fields, now validated and effective rather than record-only. Both
// windows are resolved as a pair even when the request names one, because AWS's
// non-overlap rule is about the pair: a request moving the backup window onto the
// stored maintenance window has to be rejected too.
func (s *Service) planBackupSettings(input *rds.ModifyDBInstanceInput, rec *DBInstanceRecord, plan *modifyPlan) error {
	if input.BackupRetentionPeriod != nil {
		days := aws.Int64Value(input.BackupRetentionPeriod)
		if err := s.validateRetentionPeriod(days); err != nil {
			return err
		}
		if days != rec.BackupRetentionPeriod {
			plan.BackupRetentionPeriod = aws.Int64(days)
		}
	}

	backup := aws.StringValue(input.PreferredBackupWindow)
	maintenance := aws.StringValue(input.PreferredMaintenanceWindow)
	if backup == "" && maintenance == "" {
		return nil
	}
	named := struct{ backup, maintenance bool }{backup != "", maintenance != ""}
	if backup == "" {
		backup = rec.PreferredBackupWindow
	}
	if maintenance == "" {
		maintenance = rec.PreferredMaintenanceWindow
	}
	resolvedBackup, resolvedMaintenance, err := s.validateWindows(rec.DBInstanceIdentifier, backup, maintenance)
	if err != nil {
		return err
	}
	// Only a window the request named is planned. The other side of the pair was
	// resolved for the overlap check alone, and on a record that carries none that
	// is an assignment — persisting it would report a window back to a customer who
	// never set one, and show as drift in their next plan.
	//
	// Compared against the stored value in canonical form, so a request repeating
	// the current window in different text is dropped rather than rewritten.
	if named.backup && resolvedBackup != rec.PreferredBackupWindow {
		plan.PreferredBackupWindow = resolvedBackup
	}
	if named.maintenance && resolvedMaintenance != rec.PreferredMaintenanceWindow {
		plan.PreferredMaintenanceWindow = resolvedMaintenance
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
	// leaving cleartext queued for a later window.
	if plan.MasterUserPassword != "" {
		if err := s.setMasterPassword(ctx, accountID, rec.DBInstanceIdentifier, rec.MasterUsername, plan.MasterUserPassword); err != nil {
			return awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
				"the master password could not be applied: %w", err)
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
		if plan.AutoMinorVersionUpgrade != nil {
			stored.AutoMinorVersionUpgrade = *plan.AutoMinorVersionUpgrade
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

	// Turning automated backups off has to remove the backups too, or the setting
	// is cosmetic: while any snapshot of the data volume survives, viperblock's
	// chunk GC stays latched off and the volume never reclaims an overwritten
	// chunk. It takes effect on the volume's next attach, because that guard is
	// cached for the life of the VB process.
	if plan.BackupRetentionPeriod != nil && *plan.BackupRetentionPeriod == 0 {
		s.disableAutomatedBackups(ctx, kv, accountID, rec.DBInstanceIdentifier)
	}
	return nil
}

// Sweeps an instance's automated backups after a BackupRetentionPeriod=0 modify.
// The retention change itself has already landed, so a sweep that cannot finish
// is reported rather than allowed to fail the modify: the retention reaper reads
// the same zero retention and removes whatever is left, including the newest.
func (s *Service) disableAutomatedBackups(ctx context.Context, kv jetstream.KeyValue, accountID, id string) {
	if err := s.purgeAutomatedBackups(ctx, kv, accountID, id); err != nil {
		slog.WarnContext(ctx, "rds: sweeping the automated backups of a disabled instance failed; the retention reaper will finish it",
			"dbInstance", id, "accountId", accountID, "err", err)
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, id,
			"Automated backups are disabled; the existing automated backups are still being removed.",
			EventCategoryBackup, EventCategoryNotification)
		return
	}
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, id,
		"Automated backups are disabled and the existing automated backups have been removed.",
		EventCategoryBackup, EventCategoryConfigurationChange)
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

// A supported action carrying an unimplemented parameter must not silently
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
