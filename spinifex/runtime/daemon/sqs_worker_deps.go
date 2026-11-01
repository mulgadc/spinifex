package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awsiam "github.com/aws/aws-sdk-go/service/iam"
	sqsv1 "github.com/mulgadc/spinifex/contracts/sqs/v1"
	ebsencryption "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/encryption"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
)

// sqsRole is one SQS system instance role: its name, the inline policy that
// grants exactly its internal actions, and the prefix its errors carry.
type sqsRole struct {
	name, policyName, description, label string
	actions                              []string
}

var (
	sqsWorkerRole = sqsRole{name: workerrole.RoleName, policyName: "spinifex-sqs-worker-internal",
		description: "Spinifex-managed SQS worker instance role", label: "sqs worker", actions: workerrole.Actions()}
	sqsStoreRole = sqsRole{name: sqsv1.RoleName, policyName: "spinifex-sqs-store-internal",
		description: "Spinifex-managed SQS store instance role", label: "sqs store", actions: sqsv1.Actions()}
)

// policy grants exactly the role's internal actions, built from the contract
// the gateway gate also reads, so they cannot disagree.
func (r sqsRole) policy() (string, error) {
	actions := make(iamdomain.StringOrArr, 0, len(r.actions))
	for _, a := range r.actions {
		actions = append(actions, workerrole.ServiceName+":"+a)
	}
	doc, err := json.Marshal(iamdomain.PolicyDocument{
		Version:   "2012-10-17",
		Statement: []iamdomain.Statement{{Effect: iamdomain.PolicyEffectAllow, Action: actions, Resource: iamdomain.StringOrArr{"*"}}},
	})
	if err != nil {
		return "", fmt.Errorf("marshal %s role policy: %w", r.label, err)
	}
	return string(doc), nil
}

// sqsWorkerRoles ensures the worker's instance role and profile in the system
// account. A pre-existing role is used only if its trust admits EC2 alone,
// since the gateway gate trusts STS to name every session after the instance.
type sqsWorkerRoles struct {
	provider func() iamdomain.SystemInstanceRoleEnsurer
}

var _ workerlaunch.Roles = sqsWorkerRoles{}

func (r sqsWorkerRoles) EnsureWorkerProfile(context.Context) (string, error) {
	return ensureSQSRoleProfile(r.provider, sqsWorkerRole)
}

// sqsStoreRoles ensures the store's instance role and profile, under the same
// trust and profile checks as the worker's.
type sqsStoreRoles struct {
	provider func() iamdomain.SystemInstanceRoleEnsurer
}

var _ storelaunch.Roles = sqsStoreRoles{}

func (r sqsStoreRoles) EnsureStoreProfile(context.Context) (string, error) {
	return ensureSQSRoleProfile(r.provider, sqsStoreRole)
}

func ensureSQSRoleProfile(provider func() iamdomain.SystemInstanceRoleEnsurer, role sqsRole) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("%s: IAM provider is not configured", role.label)
	}
	svc := provider()
	if svc == nil {
		return "", fmt.Errorf("%s: IAM service is unavailable", role.label)
	}
	policy, err := role.policy()
	if err != nil {
		return "", err
	}
	if err := ensureSQSRoleTrust(svc, role); err != nil {
		return "", err
	}
	acct, name := awsidentifiers.GlobalAccountID, aws.String(role.name)
	if _, err := svc.PutRolePolicy(acct, &awsiam.PutRolePolicyInput{RoleName: name,
		PolicyName: aws.String(role.policyName), PolicyDocument: aws.String(policy)}); err != nil {
		return "", fmt.Errorf("%s: put role policy: %w", role.label, err)
	}
	return ensureSQSRoleInstanceProfile(svc, role)
}

// ensureSQSRoleTrust creates the role with EC2-only trust when absent and
// verifies the trust of whatever role exists, including one a racing create
// made, before any policy is granted to it.
func ensureSQSRoleTrust(svc iamdomain.SystemInstanceRoleEnsurer, r sqsRole) error {
	acct, name := awsidentifiers.GlobalAccountID, aws.String(r.name)
	role, err := svc.GetRole(acct, &awsiam.GetRoleInput{RoleName: name})
	if err != nil {
		if err.Error() != awserrors.ErrorIAMNoSuchEntity {
			return fmt.Errorf("%s: read role: %w", r.label, err)
		}
		_, cerr := svc.CreateRole(acct, &awsiam.CreateRoleInput{RoleName: name,
			AssumeRolePolicyDocument: aws.String(iamdomain.EC2InstanceTrustPolicy),
			Description:              aws.String(r.description)})
		if cerr != nil && cerr.Error() != awserrors.ErrorIAMEntityAlreadyExists {
			return fmt.Errorf("%s: create role: %w", r.label, cerr)
		}
		if role, err = svc.GetRole(acct, &awsiam.GetRoleInput{RoleName: name}); err != nil {
			return fmt.Errorf("%s: re-read role: %w", r.label, err)
		}
	}
	if role == nil || role.Role == nil || !ec2OnlyTrust(aws.StringValue(role.Role.AssumeRolePolicyDocument)) {
		return fmt.Errorf("%s: role %s trusts more than the EC2 service", r.label, r.name)
	}
	return nil
}

// ensureSQSRoleInstanceProfile find-or-creates the profile, attaches the role
// to an empty one, and returns its ARN only once a re-read shows it carries
// exactly that role; a profile holding any other role is refused, never changed.
func ensureSQSRoleInstanceProfile(svc iamdomain.SystemInstanceRoleEnsurer, r sqsRole) (string, error) {
	acct, name := awsidentifiers.GlobalAccountID, aws.String(r.name)
	get := func() (*awsiam.InstanceProfile, error) {
		out, err := svc.GetInstanceProfile(acct, &awsiam.GetInstanceProfileInput{InstanceProfileName: name})
		if err != nil {
			return nil, err
		}
		if out == nil || out.InstanceProfile == nil {
			return nil, errors.New("empty instance profile")
		}
		return out.InstanceProfile, nil
	}
	prof, err := get()
	if err != nil {
		if err.Error() != awserrors.ErrorIAMNoSuchEntity {
			return "", fmt.Errorf("%s: read instance profile: %w", r.label, err)
		}
		if _, cerr := svc.CreateInstanceProfile(acct, &awsiam.CreateInstanceProfileInput{InstanceProfileName: name}); cerr != nil &&
			cerr.Error() != awserrors.ErrorIAMEntityAlreadyExists {
			return "", fmt.Errorf("%s: create instance profile: %w", r.label, cerr)
		}
		if prof, err = get(); err != nil {
			return "", fmt.Errorf("%s: re-read instance profile: %w", r.label, err)
		}
	}
	if !sqsProfileRoles(prof, r.name, true) {
		return "", fmt.Errorf("%s: instance profile %s carries a role other than %s", r.label, r.name, r.name)
	}
	if len(prof.Roles) == 0 {
		if _, aerr := svc.AddRoleToInstanceProfile(acct, &awsiam.AddRoleToInstanceProfileInput{
			InstanceProfileName: name, RoleName: name}); aerr != nil && aerr.Error() != awserrors.ErrorIAMLimitExceeded {
			return "", fmt.Errorf("%s: add role to instance profile: %w", r.label, aerr)
		}
		if prof, err = get(); err != nil {
			return "", fmt.Errorf("%s: re-read instance profile: %w", r.label, err)
		}
	}
	if !sqsProfileRoles(prof, r.name, false) {
		return "", fmt.Errorf("%s: instance profile %s does not carry exactly %s", r.label, r.name, r.name)
	}
	if strings.TrimSpace(aws.StringValue(prof.Arn)) == "" {
		return "", fmt.Errorf("%s: instance profile has no ARN", r.label)
	}
	return aws.StringValue(prof.Arn), nil
}

// sqsProfileRoles reports whether p carries only the named role; an empty
// profile qualifies only when allowEmpty is set.
func sqsProfileRoles(p *awsiam.InstanceProfile, role string, allowEmpty bool) bool {
	if len(p.Roles) == 0 {
		return allowEmpty
	}
	return len(p.Roles) == 1 && p.Roles[0] != nil && aws.StringValue(p.Roles[0].RoleName) == role
}

// ec2OnlyTrust reports whether doc is, as JSON, exactly the EC2 instance trust
// policy. IAM may return the document URL-encoded.
func ec2OnlyTrust(doc string) bool {
	if dec, err := url.QueryUnescape(doc); err == nil {
		doc = dec
	}
	var got, want any
	if json.Unmarshal([]byte(doc), &got) != nil || json.Unmarshal([]byte(iamdomain.EC2InstanceTrustPolicy), &want) != nil {
		return false
	}
	return reflect.DeepEqual(got, want)
}

// sqsWorkerVolumeService is the volume surface the worker needs: create via
// the store preparer, and its own record for the encryption check.
type sqsWorkerVolumeService interface {
	DescribeVolumes(ctx context.Context, input *ec2.DescribeVolumesInput, accountID string) (*ec2.DescribeVolumesOutput, error)
}

// sqsWorkerVolumes prepares the worker's key disk with the normal fresh-volume
// preparer and reads encryption from the volume's own record.
type sqsWorkerVolumes struct {
	prep             *storevolume.Preparer
	volumes          sqsWorkerVolumeService
	availabilityZone string
	sizeGiB          int64

	encryptionKeyFile string
}

var _ workerlaunch.Volumes = sqsWorkerVolumes{}

func (v sqsWorkerVolumes) Prepare(ctx context.Context) (string, error) {
	if v.prep == nil || v.availabilityZone == "" || v.sizeGiB <= 0 {
		return "", fmt.Errorf("%w: sqs worker: volume preparer is not configured", storevolume.ErrNotCreated)
	}
	// Refuse before allocating: on a cluster without a storage key every
	// attempt would otherwise strand an unencrypted volume.
	enabled, err := ebsencryption.Enabled(v.encryptionKeyFile)
	if err != nil {
		return "", fmt.Errorf("%w: sqs worker: read cluster storage key: %w", storevolume.ErrNotCreated, err)
	}
	if !enabled {
		return "", fmt.Errorf("%w: %w: the cluster storage key is not configured", storevolume.ErrNotCreated, workerlaunch.ErrUnencrypted)
	}
	return v.prep.Prepare(ctx, storevolume.Request{
		AccountID: awsidentifiers.GlobalAccountID, AvailabilityZone: v.availabilityZone, SizeGiB: v.sizeGiB,
	})
}

func (v sqsWorkerVolumes) Encrypted(ctx context.Context, volumeID string) (bool, error) {
	if v.volumes == nil {
		return false, errors.New("sqs worker: volume service is not configured")
	}
	out, err := v.volumes.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []*string{aws.String(volumeID)}},
		awsidentifiers.GlobalAccountID)
	if err != nil {
		return false, err
	}
	if out == nil || len(out.Volumes) != 1 || aws.StringValue(out.Volumes[0].VolumeId) != volumeID {
		return false, fmt.Errorf("volume %s not found in the system account", volumeID)
	}
	return aws.BoolValue(out.Volumes[0].Encrypted), nil
}

// sqsWorkerVMs launches the worker on this node through the daemon's real
// direct-boot path, which re-checks the volume's own metadata.
type sqsWorkerVMs struct {
	launch func(ctx context.Context, in *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error)
}

var _ workerlaunch.VMLauncher = sqsWorkerVMs{}

func (m sqsWorkerVMs) LaunchWorker(ctx context.Context, spec workerlaunch.Spec) (string, error) {
	if m.launch == nil {
		return "", errors.New("sqs worker: launcher is not configured")
	}
	out, err := m.launch(ctx, sqsWorkerInput(spec))
	if err != nil {
		return "", err
	}
	if out == nil || out.InstanceID == "" {
		return "", errors.New("sqs worker: launch returned no instance ID")
	}
	return out.InstanceID, nil
}

// sqsWorkerInput maps a worker spec onto the system-instance launch: its VPC
// ENI as the default NIC and an unallocated mgmt NIC the daemon fills in.
func sqsWorkerInput(spec workerlaunch.Spec) *elbv2domain.SystemInstanceInput {
	return &elbv2domain.SystemInstanceInput{
		BootMode:              systeminstance.BootDirect,
		ManagedBy:             tags.ManagedBySQS,
		InstanceType:          spec.InstanceType,
		SubnetID:              spec.Network.SubnetID,
		ENIID:                 spec.Network.ENIID,
		ENIMac:                spec.Network.ENIMac,
		ENIIP:                 spec.Network.ENIIP,
		AccountID:             awsidentifiers.GlobalAccountID,
		IamInstanceProfileArn: spec.ProfileARN,
		NICs: []systeminstance.NICConfig{
			{MAC: spec.Network.ENIMac, CIDR: spec.Network.CIDR, Gateway: spec.Network.Gateway, IsDefault: true},
			{},
		},
		Kind: systeminstance.KindSQSWorker,
		SQSWorker: &systeminstance.SQSWorkerInput{
			DataVolumeID: spec.DataVolumeID, Region: spec.Region,
		},
	}
}
