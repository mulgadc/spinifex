package handlers_ecs

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

const (
	// defaultCapacityInstanceType is the EC2 type used when the caller omits one.
	defaultCapacityInstanceType = "t3.small"

	// maxCapacityCount caps the instances a single ProvisionCapacity launches.
	maxCapacityCount = 10

	// ecsClusterTagKey associates a container instance with its cluster while
	// keeping the instance customer-owned (no system ManagedBy tag).
	ecsClusterTagKey = "spinifex:ecs-cluster"
)

// ErrECSNodeAMINotFound is returned when no spinifex-ecs-node AMI resolves for
// the account. Callers translate it to the AWS shape at the service boundary.
var ErrECSNodeAMINotFound = errors.New("ecs: spinifex-ecs-node AMI not found")

// ProvisionCapacityInput requests N container instances into a cluster.
type ProvisionCapacityInput struct {
	Cluster         string
	InstanceType    string
	Count           int
	SubnetID        string
	SecurityGroupID string
	KeyName         string
}

// ProvisionCapacityOutput returns the launched instance IDs.
type ProvisionCapacityOutput struct {
	InstanceIDs []string
}

// ProvisionCapacity launches container instances into a cluster: it ensures the
// ECS instance role/profile, resolves the spinifex-ecs-node AMI, renders
// keyless user-data (IMDS instance-role creds), and launches via the customer
// RunInstances path with the profile attached and a cluster-association tag.
func (s *Service) ProvisionCapacity(input *ProvisionCapacityInput, accountID string) (*ProvisionCapacityOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.Cluster == "" || input.SubnetID == "" || input.SecurityGroupID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	instanceType := input.InstanceType
	if instanceType == "" {
		instanceType = defaultCapacityInstanceType
	}
	count := input.Count
	if count <= 0 {
		count = 1
	}
	if count > maxCapacityCount {
		count = maxCapacityCount
	}

	if s.deps.IAM == nil {
		return nil, errors.New("ecs: capacity provisioning requires the IAM service (master key not provisioned)")
	}
	if s.deps.Images == nil {
		return nil, errors.New("ecs: capacity provisioning requires an image resolver")
	}
	if s.deps.RunInstances == nil {
		return nil, errors.New("ecs: capacity provisioning requires a RunInstances launcher")
	}

	profileARN, err := s.ensureECSInstanceProfile(accountID)
	if err != nil {
		return nil, fmt.Errorf("ensure ECS instance profile: %w", err)
	}

	amiID, err := lookupECSNodeAMI(s.deps.Images, accountID)
	if err != nil {
		return nil, err
	}

	cluster := clusterShortName(input.Cluster)
	userData := buildContainerInstanceUserData(containerInstanceUserDataInput{
		GatewayURL:    s.deps.GatewayBaseURL,
		GatewayCACert: s.deps.GatewayCACert,
		Region:        s.region,
		ClusterName:   cluster,
	})

	runInput := &ec2.RunInstancesInput{
		ImageId:            aws.String(amiID),
		InstanceType:       aws.String(instanceType),
		MinCount:           aws.Int64(int64(count)),
		MaxCount:           aws.Int64(int64(count)),
		SubnetId:           aws.String(input.SubnetID),
		SecurityGroupIds:   aws.StringSlice([]string{input.SecurityGroupID}),
		UserData:           aws.String(userData),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Arn: aws.String(profileARN)},
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("instance"),
			Tags: []*ec2.Tag{
				{Key: aws.String("Name"), Value: aws.String("ecs-node-" + cluster)},
				{Key: aws.String(ecsClusterTagKey), Value: aws.String(cluster)},
			},
		}},
	}
	if input.KeyName != "" {
		runInput.KeyName = aws.String(input.KeyName)
	}

	res, err := s.deps.RunInstances(runInput, accountID)
	if err != nil {
		return nil, err
	}

	out := &ProvisionCapacityOutput{}
	for _, inst := range res.Instances {
		if id := aws.StringValue(inst.InstanceId); id != "" {
			out.InstanceIDs = append(out.InstanceIDs, id)
		}
	}
	return out, nil
}

// lookupECSNodeAMI resolves the spinifex-ecs-node AMI by the
// spinifex:managed-by=ecs tag rather than a brittle exact name. The newest
// matching image (by CreationDate) wins.
func lookupECSNodeAMI(amiSvc ecsImageResolver, accountID string) (string, error) {
	out, err := amiSvc.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:" + tags.ManagedByKey), Values: aws.StringSlice([]string{tags.ManagedByECS})},
		},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("describe ecs AMI (tag:%s=%s): %w", tags.ManagedByKey, tags.ManagedByECS, err)
	}

	var (
		newestID      string
		newestCreated string
		matches       int
	)
	for _, img := range out.Images {
		if img == nil || img.ImageId == nil || *img.ImageId == "" {
			continue
		}
		matches++
		// CreationDate is a fixed-width RFC3339 timestamp, so lexicographic
		// comparison orders it correctly without parsing.
		if created := aws.StringValue(img.CreationDate); newestID == "" || created > newestCreated {
			newestID, newestCreated = *img.ImageId, created
		}
	}
	if newestID == "" {
		return "", fmt.Errorf("%w (tag:%s=%s, account %s)", ErrECSNodeAMINotFound, tags.ManagedByKey, tags.ManagedByECS, accountID)
	}
	if matches > 1 {
		slog.Warn("ecs: multiple AMIs match managed-by=ecs; using newest",
			"count", matches, "imageId", newestID, "created", newestCreated)
	}
	return newestID, nil
}
