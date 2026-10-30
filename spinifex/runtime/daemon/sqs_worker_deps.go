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
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
)

const sqsWorkerRolePolicyName = "spinifex-sqs-worker-internal"

// sqsWorkerRolePolicy grants exactly the worker's internal actions, built from
// the worker contract the gateway gate also reads, so they cannot disagree.
func sqsWorkerRolePolicy() (string, error) {
	actions := make(iamdomain.StringOrArr, 0, len(workerrole.Actions()))
	for _, a := range workerrole.Actions() {
		actions = append(actions, workerrole.ServiceName+":"+a)
	}
	doc, err := json.Marshal(iamdomain.PolicyDocument{
		Version:   "2012-10-17",
		Statement: []iamdomain.Statement{{Effect: iamdomain.PolicyEffectAllow, Action: actions, Resource: iamdomain.StringOrArr{"*"}}},
	})
	if err != nil {
		return "", fmt.Errorf("marshal sqs worker role policy: %w", err)
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
	if r.provider == nil {
		return "", errors.New("sqs worker: IAM provider is not configured")
	}
	svc := r.provider()
	if svc == nil {
		return "", errors.New("sqs worker: IAM service is unavailable")
	}
	policy, err := sqsWorkerRolePolicy()
	if err != nil {
		return "", err
	}
	arn, err := iamdomain.EnsureSystemInstanceProfile(svc, awsidentifiers.GlobalAccountID,
		workerrole.RoleName, sqsWorkerRolePolicyName, policy)
	if err != nil {
		return "", fmt.Errorf("sqs worker: ensure instance profile: %w", err)
	}
	if strings.TrimSpace(arn) == "" {
		return "", errors.New("sqs worker: ensure instance profile: empty profile ARN")
	}
	role, err := svc.GetRole(awsidentifiers.GlobalAccountID, &awsiam.GetRoleInput{RoleName: aws.String(workerrole.RoleName)})
	if err != nil {
		return "", fmt.Errorf("sqs worker: read role trust: %w", err)
	}
	if role == nil || role.Role == nil || !ec2OnlyTrust(aws.StringValue(role.Role.AssumeRolePolicyDocument)) {
		return "", fmt.Errorf("sqs worker: role %s trusts more than the EC2 service", workerrole.RoleName)
	}
	return arn, nil
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
}

var _ workerlaunch.Volumes = sqsWorkerVolumes{}

func (v sqsWorkerVolumes) Prepare(ctx context.Context) (string, error) {
	if v.prep == nil || v.availabilityZone == "" || v.sizeGiB <= 0 {
		return "", errors.New("sqs worker: volume preparer is not configured")
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
			DataVolumeID: spec.DataVolumeID, StoreStatus: spec.StoreStatus, StoreNKey: spec.StoreNKey,
		},
	}
}
