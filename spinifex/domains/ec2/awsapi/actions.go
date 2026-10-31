// Package awsapi serves the EC2 query-protocol actions over composed capabilities.
package awsapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"uuid"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2accountapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/account"
	ec2capacityreservationapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/capacityreservation"
	ec2eigwapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/eigw"
	ec2eipapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/eip"
	ec2igwapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/igw"
	ec2imageapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/image"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	ec2keyapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/key"
	ec2launchtemplateapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/launchtemplate"
	ec2natgwapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/natgw"
	ec2placementgroupapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/placementgroup"
	ec2routetableapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/routetable"
	ec2snapshotapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/snapshot"
	ec2spotinstanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/spotinstance"
	ec2tagsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/tags"
	ec2volumeapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/volume"
	ec2vpcapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/vpc"
	ec2zoneapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/zone"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsxml "github.com/mulgadc/spinifex/spinifex/foundation/aws/xml"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/idempotency"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/nats-io/nats.go"
)

// ec2Action parses once so authorization and dispatch share one typed input.
type ec2Action struct {
	parse    func(map[string]string) (any, error)
	dispatch func(ctx context.Context, action string, parsed any, c *call, accountID string) ([]byte, error)
	// idempotent records that this action honours ClientToken, so the set of
	// actions that do is assertable rather than folklore.
	idempotent bool
}

func (h ec2Action) withQueryPreprocessor(preprocess func(map[string]string)) ec2Action {
	parse := h.parse
	h.parse = func(q map[string]string) (any, error) {
		preprocess(q)
		return parse(q)
	}
	return h
}

// requestContext returns r's context, or Background for a nil request
// (some callers and tests dispatch without an *http.Request). The SDK's
// invocation id rides along: it is repeated across retries of one call, so it
// is what lets a mutation recognise its own resend instead of doing the work
// twice.
func requestContext(r *http.Request) context.Context {
	if r == nil {
		return context.Background()
	}
	return idempotency.WithKey(r.Context(), r.Header.Get(idempotency.SDKInvocationIDHeader))
}

// parseEC2Input parses query args into an action's input type. Shared by every
// ec2Action constructor so they all consume the same parse.
func parseEC2Input[In any](q map[string]string) (any, error) {
	input := new(In)
	if err := query.QueryParamsToStruct(q, input); err != nil {
		return nil, err
	}
	return input, nil
}

// ec2Handler creates a type-safe EC2Handler. Parsing is separate from dispatch
// so authorization and execution consume the exact same input.
func ec2Handler[In any](handler func(ctx context.Context, input *In, c *call, accountID string) (any, error)) ec2Action {
	return ec2Action{
		parse: parseEC2Input[In],
		dispatch: func(ctx context.Context, action string, parsed any, c *call, accountID string) ([]byte, error) {
			input, ok := parsed.(*In)
			if !ok {
				return nil, errors.New(awserrors.ErrorInternalError)
			}
			output, err := handler(ctx, input, c, accountID)
			if err != nil {
				return nil, err
			}
			xmlOutput, err := marshalEC2Response(action, output)
			if err != nil {
				return nil, err
			}
			return xmlOutput, nil
		},
	}
}

// ec2ListsAsSet names the list fields AWS omits when empty instead of
// rendering them. Each is rendered as its handler set it, so nil omits it.
var ec2ListsAsSet = map[reflect.Type][]string{
	reflect.TypeFor[ec2.CreateKeyPairOutput]():              {"Tags"},
	reflect.TypeFor[ec2.Image]():                            {"Tags", "ProductCodes"},
	reflect.TypeFor[ec2.CreateSecurityGroupOutput]():        {"Tags"},
	reflect.TypeFor[ec2.SecurityGroup]():                    {"Tags"},
	reflect.TypeFor[ec2.SecurityGroupRule]():                {"Tags"},
	reflect.TypeFor[ec2.RevokeSecurityGroupIngressOutput](): {"UnknownIpPermissions"},
	reflect.TypeFor[ec2.RevokeSecurityGroupEgressOutput]():  {"UnknownIpPermissions"},
	reflect.TypeFor[ec2.NetworkInterface]():                 {"Ipv4Prefixes", "Ipv6Prefixes"},
	reflect.TypeFor[ec2.Vpc]():                              {"Ipv6CidrBlockAssociationSet"},
	reflect.TypeFor[ec2.Subnet]():                           {"Tags"},
}

// marshalEC2Response renders an EC2 handler's output into the action's XML
// envelope. Every EC2 action funnels through here (via ec2Handler and
// ec2HandlerWithReq), so wire-format fixes belong here, not in each handler.
func marshalEC2Response(action string, output any) ([]byte, error) {
	// BuildXML omits a nil slice's container element entirely but renders an
	// empty one for a non-nil empty slice; AWS mostly renders the latter.
	normalized := awsxml.NormalizeOutput(output, ec2ListsAsSet)
	// The SDK's generated output structs never carry a RequestId field.
	withRequestID := awsxml.WithRequestID(normalized, uuid.NewV4().String())
	payload := awsxml.ResponsePayload(action+"Response", withRequestID)
	xmlOutput, err := awsxml.Marshal(payload)
	if err != nil {
		return nil, errors.New("failed to marshal response to XML")
	}
	return xmlOutput, nil
}

var ec2Actions = map[string]ec2Action{
	"DescribeInstances": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstancesInput, c *call, accountID string) (any, error) {
		out, err := ec2instanceapi.DescribeInstancesChecked(ctx, input, c.NATS, 0, c.NodeIDs, accountID)
		if err != nil {
			return out, err
		}
		ec2instanceapi.EnrichInstanceProfileIDs(out, c.IAM, accountID)
		return out, nil
	}),
	"RunInstances": ec2Handler(func(ctx context.Context, input *ec2.RunInstancesInput, c *call, accountID string) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return c.passRole(roleARN)
		}
		launchQuotaCheck := func() error {
			return c.Quota.EnforceLaunch(ctx, accountID, aws.StringValue(input.InstanceType), int(aws.Int64Value(input.MaxCount)))
		}
		reservation, err := ec2instanceapi.RunInstances(ctx, input, c.NATS, c.IAM, accountID, passRoleCheck, launchQuotaCheck, c.ExpectedNodes)
		if err != nil {
			return nil, err
		}
		// Charge the actual launched vCPUs; a counter write failure is drift for
		// reconcile to correct, so it must not fail the already-successful launch.
		if err := c.Quota.ChargeLaunch(ctx, accountID, &reservation); err != nil {
			slog.WarnContext(ctx, "RunInstances: vcpu quota charge failed, reconcile will correct", "account", accountID, "err", err)
		}
		return reservation, nil
	}),
	"AssociateIamInstanceProfile": ec2Handler(func(ctx context.Context, input *ec2.AssociateIamInstanceProfileInput, c *call, accountID string) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return c.passRole(roleARN)
		}
		return ec2instanceapi.AssociateIamInstanceProfile(ctx, input, c.NATS, c.IAM, accountID, passRoleCheck)
	}),
	"DisassociateIamInstanceProfile": ec2Handler(func(ctx context.Context, input *ec2.DisassociateIamInstanceProfileInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.DisassociateIamInstanceProfile(ctx, input, c.NATS, c.ActiveNodes(ctx), accountID)
	}),
	"ReplaceIamInstanceProfileAssociation": ec2Handler(func(ctx context.Context, input *ec2.ReplaceIamInstanceProfileAssociationInput, c *call, accountID string) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return c.passRole(roleARN)
		}
		return ec2instanceapi.ReplaceIamInstanceProfileAssociation(ctx, input, c.NATS, c.IAM, c.ActiveNodes(ctx), accountID, passRoleCheck)
	}),
	"DescribeIamInstanceProfileAssociations": ec2Handler(func(ctx context.Context, input *ec2.DescribeIamInstanceProfileAssociationsInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.DescribeIamInstanceProfileAssociations(ctx, input, c.NATS, c.ActiveNodes(ctx), accountID)
	}),
	"StartInstances": ec2Handler(func(ctx context.Context, input *ec2.StartInstancesInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.StartInstances(ctx, input, c.NATS, accountID)
	}),
	"StopInstances": ec2Handler(func(ctx context.Context, input *ec2.StopInstancesInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.StopInstances(ctx, input, c.NATS, accountID)
	}),
	"RebootInstances": ec2Handler(func(ctx context.Context, input *ec2.RebootInstancesInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.RebootInstances(ctx, input, c.NATS, accountID)
	}),
	"MonitorInstances": ec2Handler(func(ctx context.Context, input *ec2.MonitorInstancesInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.MonitorInstances(ctx, input, c.NATS, accountID)
	}),
	"UnmonitorInstances": ec2Handler(func(ctx context.Context, input *ec2.UnmonitorInstancesInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.UnmonitorInstances(ctx, input, c.NATS, accountID)
	}),
	"TerminateInstances": ec2Handler(func(ctx context.Context, input *ec2.TerminateInstancesInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.TerminateInstances(ctx, input, c.NATS, accountID)
	}),
	"DescribeInstanceTypes": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceTypesInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.DescribeInstanceTypes(ctx, input, c.NATS, c.ExpectedNodes, c.NodeIDs, accountID)
	}),
	"DescribeInstanceTypeOfferings": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceTypeOfferingsInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.DescribeInstanceTypeOfferings(ctx, input, c.NATS, c.ExpectedNodes, accountID, c.Region, c.AZ)
	}),
	"DescribeInstanceStatus": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceStatusInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.DescribeInstanceStatus(ctx, input, c.NATS, c.ActiveNodes(ctx), accountID, c.AZ, c.InstanceStatus)
	}),
	"GetConsoleOutput": ec2Handler(func(ctx context.Context, input *ec2.GetConsoleOutputInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.GetConsoleOutput(ctx, input, c.NATS, accountID)
	}),
	"GetPasswordData": ec2Handler(func(ctx context.Context, input *ec2.GetPasswordDataInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.GetPasswordData(ctx, input, c.NATS, accountID)
	}),
	"ModifyInstanceAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyInstanceAttributeInput, c *call, accountID string) (any, error) {
		var delta int
		if input.InstanceType != nil {
			d, err := c.Quota.EnforceRetype(ctx, c.NATS, func() []string { return c.NodeIDs }, accountID, aws.StringValue(input.InstanceId), aws.StringValue(input.InstanceType.Value))
			if err != nil {
				return nil, err
			}
			delta = d
		}
		out, err := ec2instanceapi.ModifyInstanceAttribute(ctx, input, c.NATS, accountID)
		if err != nil {
			return nil, err
		}
		// Charge the retype's vCPU growth; a counter write failure is drift for
		// reconcile to correct, so it must not fail the applied retype.
		if err := c.Quota.AddVCPU(ctx, accountID, delta); err != nil {
			slog.WarnContext(ctx, "ModifyInstanceAttribute: vcpu quota charge failed, reconcile will correct", "account", accountID, "err", err)
		}
		return out, nil
	}),
	"DescribeInstanceAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceAttributeInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.DescribeInstanceAttribute(ctx, input, c.NATS, c.ActiveNodes(ctx), accountID)
	}),
	"ModifyInstanceMetadataOptions": ec2Handler(func(ctx context.Context, input *ec2.ModifyInstanceMetadataOptionsInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.ModifyInstanceMetadataOptions(ctx, input, c.NATS, accountID)
	}),
	"DescribeInstanceCreditSpecifications": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceCreditSpecificationsInput, c *call, accountID string) (any, error) {
		return ec2instanceapi.DescribeInstanceCreditSpecifications(input)
	}),
	"CreateKeyPair": ec2Handler(func(ctx context.Context, input *ec2.CreateKeyPairInput, c *call, accountID string) (any, error) {
		return ec2keyapi.CreateKeyPair(ctx, input, c.NATS, accountID)
	}),
	"DeleteKeyPair": ec2Handler(func(ctx context.Context, input *ec2.DeleteKeyPairInput, c *call, accountID string) (any, error) {
		return ec2keyapi.DeleteKeyPair(ctx, input, c.NATS, accountID)
	}),
	"DescribeKeyPairs": ec2Handler(func(ctx context.Context, input *ec2.DescribeKeyPairsInput, c *call, accountID string) (any, error) {
		return ec2keyapi.DescribeKeyPairs(ctx, input, c.NATS, accountID)
	}),
	"ImportKeyPair": ec2Handler(func(ctx context.Context, input *ec2.ImportKeyPairInput, c *call, accountID string) (any, error) {
		return ec2keyapi.ImportKeyPair(ctx, input, c.NATS, accountID)
	}).withQueryPreprocessor(func(q map[string]string) {
		// Parser leaves Base64 padding URL-encoded; decode it before dispatch.
		if strings.HasSuffix(q["PublicKeyMaterial"], "%3D%3D") {
			q["PublicKeyMaterial"] = strings.Replace(q["PublicKeyMaterial"], "%3D%3D", "==", 1)
		}
	}),
	"DescribeImages": ec2Handler(func(ctx context.Context, input *ec2.DescribeImagesInput, c *call, accountID string) (any, error) {
		return ec2imageapi.DescribeImages(ctx, input, c.NATS, accountID)
	}),
	"CreateImage": ec2Handler(func(ctx context.Context, input *ec2.CreateImageInput, c *call, accountID string) (any, error) {
		return ec2imageapi.CreateImage(ctx, input, c.NATS, c.ActiveNodes(ctx), accountID)
	}),
	"DeregisterImage": ec2Handler(func(ctx context.Context, input *ec2.DeregisterImageInput, c *call, accountID string) (any, error) {
		return ec2imageapi.DeregisterImage(ctx, input, c.NATS, accountID)
	}),
	"RegisterImage": ec2Handler(func(ctx context.Context, input *ec2.RegisterImageInput, c *call, accountID string) (any, error) {
		return ec2imageapi.RegisterImage(ctx, input, c.NATS, accountID)
	}),
	"CopyImage": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CopyImageInput, c *call, accountID string) (ec2.CopyImageOutput, error) {
		return ec2imageapi.CopyImage(ctx, input, c.NATS, c.Region, accountID)
	}),
	"DescribeImageAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeImageAttributeInput, c *call, accountID string) (any, error) {
		return ec2imageapi.DescribeImageAttribute(ctx, input, c.NATS, accountID)
	}),
	"ModifyImageAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyImageAttributeInput, c *call, accountID string) (any, error) {
		return ec2imageapi.ModifyImageAttribute(ctx, input, c.NATS, accountID)
	}),
	"ResetImageAttribute": ec2Handler(func(ctx context.Context, input *ec2.ResetImageAttributeInput, c *call, accountID string) (any, error) {
		return ec2imageapi.ResetImageAttribute(ctx, input, c.NATS, accountID)
	}),
	"DescribeRegions": ec2Handler(func(ctx context.Context, input *ec2.DescribeRegionsInput, c *call, accountID string) (any, error) {
		return ec2zoneapi.DescribeRegions(input, c.Region, c.Endpoint)
	}),
	"DescribeAvailabilityZones": ec2Handler(func(ctx context.Context, input *ec2.DescribeAvailabilityZonesInput, c *call, accountID string) (any, error) {
		return ec2zoneapi.DescribeAvailabilityZones(input, c.Region, c.AZ)
	}),
	"DescribeVolumes": ec2Handler(func(ctx context.Context, input *ec2.DescribeVolumesInput, c *call, accountID string) (any, error) {
		return ec2volumeapi.DescribeVolumes(ctx, input, c.NATS, accountID)
	}),
	"ModifyVolume": ec2Handler(func(ctx context.Context, input *ec2.ModifyVolumeInput, c *call, accountID string) (any, error) {
		if err := c.Quota.EnforceVolumeModify(ctx, c.NATS, accountID, aws.StringValue(input.VolumeId), int(aws.Int64Value(input.Size))); err != nil {
			return nil, err
		}
		return ec2volumeapi.ModifyVolume(ctx, input, c.NATS, accountID)
	}),
	"CreateVolume": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CreateVolumeInput, c *call, accountID string) (ec2.Volume, error) {
		if err := c.Quota.EnforceVolumeCreate(ctx, c.NATS, accountID, int(aws.Int64Value(input.Size))); err != nil {
			return ec2.Volume{}, err
		}
		return ec2volumeapi.CreateVolume(ctx, input, c.NATS, accountID)
	}),
	"DeleteVolume": ec2Handler(func(ctx context.Context, input *ec2.DeleteVolumeInput, c *call, accountID string) (any, error) {
		return ec2volumeapi.DeleteVolume(ctx, input, c.NATS, c.NodeIDs, accountID)
	}),
	"AttachVolume": ec2Handler(func(ctx context.Context, input *ec2.AttachVolumeInput, c *call, accountID string) (any, error) {
		return ec2volumeapi.AttachVolume(ctx, input, c.NATS, accountID)
	}),
	"DescribeVolumeStatus": ec2Handler(func(ctx context.Context, input *ec2.DescribeVolumeStatusInput, c *call, accountID string) (any, error) {
		return ec2volumeapi.DescribeVolumeStatus(ctx, input, c.NATS, accountID)
	}),
	"DescribeVolumesModifications": ec2Handler(func(ctx context.Context, input *ec2.DescribeVolumesModificationsInput, c *call, accountID string) (any, error) {
		return ec2volumeapi.DescribeVolumesModifications(ctx, input, c.NATS, accountID)
	}),
	"DetachVolume": ec2Handler(func(ctx context.Context, input *ec2.DetachVolumeInput, c *call, accountID string) (any, error) {
		return ec2volumeapi.DetachVolume(ctx, input, c.NATS, accountID)
	}),
	"DescribeAccountAttributes": ec2Handler(func(ctx context.Context, input *ec2.DescribeAccountAttributesInput, c *call, accountID string) (any, error) {
		return ec2accountapi.DescribeAccountAttributes(input, func() (string, error) {
			return defaultVPCID(ctx, c.NATS, accountID)
		})
	}),
	"EnableEbsEncryptionByDefault": ec2Handler(func(ctx context.Context, input *ec2.EnableEbsEncryptionByDefaultInput, c *call, accountID string) (any, error) {
		return ec2accountapi.EnableEbsEncryptionByDefault(ctx, input, c.NATS, accountID)
	}),
	"DisableEbsEncryptionByDefault": ec2Handler(func(ctx context.Context, input *ec2.DisableEbsEncryptionByDefaultInput, c *call, accountID string) (any, error) {
		return ec2accountapi.DisableEbsEncryptionByDefault(ctx, input, c.NATS, accountID)
	}),
	"GetEbsEncryptionByDefault": ec2Handler(func(ctx context.Context, input *ec2.GetEbsEncryptionByDefaultInput, c *call, accountID string) (any, error) {
		return ec2accountapi.GetEbsEncryptionByDefault(ctx, input, c.NATS, accountID)
	}),
	"GetSerialConsoleAccessStatus": ec2Handler(func(ctx context.Context, input *ec2.GetSerialConsoleAccessStatusInput, c *call, accountID string) (any, error) {
		return ec2accountapi.GetSerialConsoleAccessStatus(ctx, input, c.NATS, accountID)
	}),
	"EnableSerialConsoleAccess": ec2Handler(func(ctx context.Context, input *ec2.EnableSerialConsoleAccessInput, c *call, accountID string) (any, error) {
		return ec2accountapi.EnableSerialConsoleAccess(ctx, input, c.NATS, accountID)
	}),
	"DisableSerialConsoleAccess": ec2Handler(func(ctx context.Context, input *ec2.DisableSerialConsoleAccessInput, c *call, accountID string) (any, error) {
		return ec2accountapi.DisableSerialConsoleAccess(ctx, input, c.NATS, accountID)
	}),
	"CreateTags": ec2Handler(func(ctx context.Context, input *ec2.CreateTagsInput, c *call, accountID string) (any, error) {
		return ec2tagsapi.CreateTags(ctx, input, c.NATS, accountID)
	}),
	"DeleteTags": ec2Handler(func(ctx context.Context, input *ec2.DeleteTagsInput, c *call, accountID string) (any, error) {
		return ec2tagsapi.DeleteTags(ctx, input, c.NATS, accountID)
	}),
	"DescribeTags": ec2Handler(func(ctx context.Context, input *ec2.DescribeTagsInput, c *call, accountID string) (any, error) {
		return ec2tagsapi.DescribeTags(ctx, input, c.NATS, accountID)
	}),
	"CreateSnapshot": ec2Handler(func(ctx context.Context, input *ec2.CreateSnapshotInput, c *call, accountID string) (any, error) {
		return ec2snapshotapi.CreateSnapshot(ctx, input, c.NATS, accountID)
	}),
	"DeleteSnapshot": ec2Handler(func(ctx context.Context, input *ec2.DeleteSnapshotInput, c *call, accountID string) (any, error) {
		return ec2snapshotapi.DeleteSnapshot(ctx, input, c.NATS, accountID)
	}),
	"DescribeSnapshots": ec2Handler(func(ctx context.Context, input *ec2.DescribeSnapshotsInput, c *call, accountID string) (any, error) {
		return ec2snapshotapi.DescribeSnapshots(ctx, input, c.NATS, accountID)
	}),
	"CopySnapshot": ec2Handler(func(ctx context.Context, input *ec2.CopySnapshotInput, c *call, accountID string) (any, error) {
		return ec2snapshotapi.CopySnapshot(ctx, input, c.NATS, accountID)
	}),
	"CreateInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.CreateInternetGatewayInput, c *call, accountID string) (any, error) {
		return ec2igwapi.CreateInternetGateway(ctx, input, c.NATS, accountID)
	}),
	"DeleteInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.DeleteInternetGatewayInput, c *call, accountID string) (any, error) {
		return ec2igwapi.DeleteInternetGateway(ctx, input, c.NATS, accountID)
	}),
	"DescribeInternetGateways": ec2Handler(func(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, c *call, accountID string) (any, error) {
		return ec2igwapi.DescribeInternetGateways(ctx, input, c.NATS, accountID)
	}),
	"AttachInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.AttachInternetGatewayInput, c *call, accountID string) (any, error) {
		return ec2igwapi.AttachInternetGateway(ctx, input, c.NATS, accountID)
	}),
	"DetachInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.DetachInternetGatewayInput, c *call, accountID string) (any, error) {
		return ec2igwapi.DetachInternetGateway(ctx, input, c.NATS, accountID)
	}),
	"CreateEgressOnlyInternetGateway": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CreateEgressOnlyInternetGatewayInput, c *call, accountID string) (ec2.CreateEgressOnlyInternetGatewayOutput, error) {
		return ec2eigwapi.CreateEgressOnlyInternetGateway(ctx, input, c.NATS, accountID)
	}),
	"DeleteEgressOnlyInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.DeleteEgressOnlyInternetGatewayInput, c *call, accountID string) (any, error) {
		return ec2eigwapi.DeleteEgressOnlyInternetGateway(ctx, input, c.NATS, accountID)
	}),
	"DescribeEgressOnlyInternetGateways": ec2Handler(func(ctx context.Context, input *ec2.DescribeEgressOnlyInternetGatewaysInput, c *call, accountID string) (any, error) {
		return ec2eigwapi.DescribeEgressOnlyInternetGateways(ctx, input, c.NATS, accountID)
	}),
	"CreatePlacementGroup": ec2Handler(func(ctx context.Context, input *ec2.CreatePlacementGroupInput, c *call, accountID string) (any, error) {
		return ec2placementgroupapi.CreatePlacementGroup(ctx, input, c.NATS, accountID)
	}),
	"DeletePlacementGroup": ec2Handler(func(ctx context.Context, input *ec2.DeletePlacementGroupInput, c *call, accountID string) (any, error) {
		return ec2placementgroupapi.DeletePlacementGroup(ctx, input, c.NATS, accountID)
	}),
	"DescribePlacementGroups": ec2Handler(func(ctx context.Context, input *ec2.DescribePlacementGroupsInput, c *call, accountID string) (any, error) {
		return ec2placementgroupapi.DescribePlacementGroups(ctx, input, c.NATS, accountID)
	}),
	"CreateCapacityReservation": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CreateCapacityReservationInput, c *call, accountID string) (ec2.CreateCapacityReservationOutput, error) {
		return ec2capacityreservationapi.CreateCapacityReservation(ctx, input, c.NATS, c.ExpectedNodes, accountID)
	}),
	"DescribeCapacityReservations": ec2Handler(func(ctx context.Context, input *ec2.DescribeCapacityReservationsInput, c *call, accountID string) (any, error) {
		return ec2capacityreservationapi.DescribeCapacityReservations(ctx, input, c.NATS, c.ExpectedNodes, accountID)
	}),
	"CancelCapacityReservation": ec2Handler(func(ctx context.Context, input *ec2.CancelCapacityReservationInput, c *call, accountID string) (any, error) {
		return ec2capacityreservationapi.CancelCapacityReservation(ctx, input, c.NATS, c.ExpectedNodes, accountID)
	}),
	"CreateLaunchTemplate": ec2Handler(func(ctx context.Context, input *ec2.CreateLaunchTemplateInput, c *call, accountID string) (any, error) {
		return ec2launchtemplateapi.CreateLaunchTemplate(ctx, input, c.NATS, accountID)
	}),
	"CreateLaunchTemplateVersion": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CreateLaunchTemplateVersionInput, c *call, accountID string) (ec2.CreateLaunchTemplateVersionOutput, error) {
		return ec2launchtemplateapi.CreateLaunchTemplateVersion(ctx, input, c.NATS, accountID)
	}),
	"DeleteLaunchTemplate": ec2Handler(func(ctx context.Context, input *ec2.DeleteLaunchTemplateInput, c *call, accountID string) (any, error) {
		return ec2launchtemplateapi.DeleteLaunchTemplate(ctx, input, c.NATS, accountID)
	}),
	"DeleteLaunchTemplateVersions": ec2Handler(func(ctx context.Context, input *ec2.DeleteLaunchTemplateVersionsInput, c *call, accountID string) (any, error) {
		return ec2launchtemplateapi.DeleteLaunchTemplateVersions(ctx, input, c.NATS, accountID)
	}),
	"ModifyLaunchTemplate": ec2Handler(func(ctx context.Context, input *ec2.ModifyLaunchTemplateInput, c *call, accountID string) (any, error) {
		return ec2launchtemplateapi.ModifyLaunchTemplate(ctx, input, c.NATS, accountID)
	}),
	"DescribeLaunchTemplates": ec2Handler(func(ctx context.Context, input *ec2.DescribeLaunchTemplatesInput, c *call, accountID string) (any, error) {
		return ec2launchtemplateapi.DescribeLaunchTemplates(ctx, input, c.NATS, accountID)
	}),
	"DescribeLaunchTemplateVersions": ec2Handler(func(ctx context.Context, input *ec2.DescribeLaunchTemplateVersionsInput, c *call, accountID string) (any, error) {
		return ec2launchtemplateapi.DescribeLaunchTemplateVersions(ctx, input, c.NATS, accountID)
	}),
	"RequestSpotInstances": ec2Handler(func(ctx context.Context, input *ec2.RequestSpotInstancesInput, c *call, accountID string) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return c.passRole(roleARN)
		}
		return ec2spotinstanceapi.RequestSpotInstances(ctx, input, c.NATS, c.IAM, accountID, c.AZ, passRoleCheck, c.Quota, c.ExpectedNodes)
	}),
	"DescribeSpotInstanceRequests": ec2Handler(func(ctx context.Context, input *ec2.DescribeSpotInstanceRequestsInput, c *call, accountID string) (any, error) {
		return ec2spotinstanceapi.DescribeSpotInstanceRequests(ctx, input, c.NATS, accountID)
	}),
	"CancelSpotInstanceRequests": ec2Handler(func(ctx context.Context, input *ec2.CancelSpotInstanceRequestsInput, c *call, accountID string) (any, error) {
		return ec2spotinstanceapi.CancelSpotInstanceRequests(ctx, input, c.NATS, accountID)
	}),
	"CreateVpc": ec2Handler(func(ctx context.Context, input *ec2.CreateVpcInput, c *call, accountID string) (any, error) {
		if err := c.Quota.EnforceVPCs(ctx, c.NATS, accountID, 1); err != nil {
			return nil, err
		}
		return ec2vpcapi.CreateVpc(ctx, input, c.NATS, accountID)
	}),
	"DeleteVpc": ec2Handler(func(ctx context.Context, input *ec2.DeleteVpcInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DeleteVpc(ctx, input, c.NATS, accountID)
	}),
	"DescribeVpcs": ec2Handler(func(ctx context.Context, input *ec2.DescribeVpcsInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DescribeVpcs(ctx, input, c.NATS, accountID)
	}),
	"ModifyVpcAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyVpcAttributeInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.ModifyVpcAttribute(ctx, input, c.NATS, accountID)
	}),
	"DescribeVpcAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeVpcAttributeInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DescribeVpcAttribute(ctx, input, c.NATS, accountID)
	}),
	// No VPC endpoint can exist here, because CreateVpcEndpoint is not served, so
	// the empty list is the true answer rather than a stand-in for one. It has to
	// be answered all the same: the Terraform AWS provider calls this on every VPC
	// delete to sweep GuardDuty-managed endpoints, and an InvalidAction there fails
	// the destroy and strands the VPC.
	"DescribeVpcEndpoints": ec2Handler(func(_ context.Context, _ *ec2.DescribeVpcEndpointsInput, _ *call, _ string) (any, error) {
		return ec2.DescribeVpcEndpointsOutput{VpcEndpoints: []*ec2.VpcEndpoint{}}, nil
	}),
	"CreateSubnet": ec2Handler(func(ctx context.Context, input *ec2.CreateSubnetInput, c *call, accountID string) (any, error) {
		if err := c.Quota.EnforceSubnets(ctx, c.NATS, accountID, 1); err != nil {
			return nil, err
		}
		return ec2vpcapi.CreateSubnet(ctx, input, c.NATS, accountID)
	}),
	"DeleteSubnet": ec2Handler(func(ctx context.Context, input *ec2.DeleteSubnetInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DeleteSubnet(ctx, input, c.NATS, accountID)
	}),
	"DescribeSubnets": ec2Handler(func(ctx context.Context, input *ec2.DescribeSubnetsInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DescribeSubnets(ctx, input, c.NATS, accountID)
	}),
	"ModifySubnetAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifySubnetAttributeInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.ModifySubnetAttribute(ctx, input, c.NATS, accountID)
	}),
	"CreateRouteTable": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CreateRouteTableInput, c *call, accountID string) (ec2.CreateRouteTableOutput, error) {
		return ec2routetableapi.CreateRouteTable(ctx, input, c.NATS, accountID)
	}),
	"DeleteRouteTable": ec2Handler(func(ctx context.Context, input *ec2.DeleteRouteTableInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.DeleteRouteTable(ctx, input, c.NATS, accountID)
	}),
	"DescribeRouteTables": ec2Handler(func(ctx context.Context, input *ec2.DescribeRouteTablesInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.DescribeRouteTables(ctx, input, c.NATS, accountID)
	}),
	"CreateRoute": ec2Handler(func(ctx context.Context, input *ec2.CreateRouteInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.CreateRoute(ctx, input, c.NATS, accountID)
	}),
	"DeleteRoute": ec2Handler(func(ctx context.Context, input *ec2.DeleteRouteInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.DeleteRoute(ctx, input, c.NATS, accountID)
	}),
	"ReplaceRoute": ec2Handler(func(ctx context.Context, input *ec2.ReplaceRouteInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.ReplaceRoute(ctx, input, c.NATS, accountID)
	}),
	"AssociateRouteTable": ec2Handler(func(ctx context.Context, input *ec2.AssociateRouteTableInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.AssociateRouteTable(ctx, input, c.NATS, accountID)
	}),
	"DisassociateRouteTable": ec2Handler(func(ctx context.Context, input *ec2.DisassociateRouteTableInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.DisassociateRouteTable(ctx, input, c.NATS, accountID)
	}),
	"ReplaceRouteTableAssociation": ec2Handler(func(ctx context.Context, input *ec2.ReplaceRouteTableAssociationInput, c *call, accountID string) (any, error) {
		return ec2routetableapi.ReplaceRouteTableAssociation(ctx, input, c.NATS, accountID)
	}),
	"CreateNetworkInterface": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, c *call, accountID string) (ec2.CreateNetworkInterfaceOutput, error) {
		return ec2vpcapi.CreateNetworkInterface(ctx, input, c.NATS, accountID)
	}),
	"DeleteNetworkInterface": ec2Handler(func(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DeleteNetworkInterface(ctx, input, c.NATS, accountID)
	}),
	"DescribeNetworkInterfaces": ec2Handler(func(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DescribeNetworkInterfaces(ctx, input, c.NATS, accountID)
	}),
	"ModifyNetworkInterfaceAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.ModifyNetworkInterfaceAttribute(ctx, input, c.NATS, accountID)
	}),
	"AttachNetworkInterface": ec2Handler(func(ctx context.Context, input *ec2.AttachNetworkInterfaceInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.AttachNetworkInterface(ctx, input, c.NATS, accountID)
	}),
	"DetachNetworkInterface": ec2Handler(func(ctx context.Context, input *ec2.DetachNetworkInterfaceInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DetachNetworkInterface(ctx, input, c.NATS, accountID)
	}),
	"CreateSecurityGroup": ec2Handler(func(ctx context.Context, input *ec2.CreateSecurityGroupInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.CreateSecurityGroup(ctx, input, c.NATS, accountID)
	}),
	"DeleteSecurityGroup": ec2Handler(func(ctx context.Context, input *ec2.DeleteSecurityGroupInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DeleteSecurityGroup(ctx, input, c.NATS, accountID)
	}),
	"DescribeSecurityGroups": ec2Handler(func(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DescribeSecurityGroups(ctx, input, c.NATS, accountID)
	}),
	"DescribeSecurityGroupRules": ec2Handler(func(ctx context.Context, input *ec2.DescribeSecurityGroupRulesInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.DescribeSecurityGroupRules(ctx, input, c.NATS, accountID)
	}),
	"GetSecurityGroupsForVpc": ec2Handler(func(ctx context.Context, input *ec2.GetSecurityGroupsForVpcInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.GetSecurityGroupsForVpc(ctx, input, c.NATS, accountID)
	}),
	"AuthorizeSecurityGroupIngress": ec2Handler(func(ctx context.Context, input *ec2.AuthorizeSecurityGroupIngressInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.AuthorizeSecurityGroupIngress(ctx, input, c.NATS, accountID)
	}),
	"AuthorizeSecurityGroupEgress": ec2Handler(func(ctx context.Context, input *ec2.AuthorizeSecurityGroupEgressInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.AuthorizeSecurityGroupEgress(ctx, input, c.NATS, accountID)
	}),
	"RevokeSecurityGroupIngress": ec2Handler(func(ctx context.Context, input *ec2.RevokeSecurityGroupIngressInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.RevokeSecurityGroupIngress(ctx, input, c.NATS, accountID)
	}),
	"RevokeSecurityGroupEgress": ec2Handler(func(ctx context.Context, input *ec2.RevokeSecurityGroupEgressInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.RevokeSecurityGroupEgress(ctx, input, c.NATS, accountID)
	}),
	"UpdateSecurityGroupRuleDescriptionsIngress": ec2Handler(func(ctx context.Context, input *ec2.UpdateSecurityGroupRuleDescriptionsIngressInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.UpdateSecurityGroupRuleDescriptionsIngress(ctx, input, c.NATS, accountID)
	}),
	"UpdateSecurityGroupRuleDescriptionsEgress": ec2Handler(func(ctx context.Context, input *ec2.UpdateSecurityGroupRuleDescriptionsEgressInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.UpdateSecurityGroupRuleDescriptionsEgress(ctx, input, c.NATS, accountID)
	}),
	"ModifySecurityGroupRules": ec2Handler(func(ctx context.Context, input *ec2.ModifySecurityGroupRulesInput, c *call, accountID string) (any, error) {
		return ec2vpcapi.ModifySecurityGroupRules(ctx, input, c.NATS, accountID)
	}),
	"AllocateAddress": ec2Handler(func(ctx context.Context, input *ec2.AllocateAddressInput, c *call, accountID string) (any, error) {
		if err := c.Quota.EnforceEIPs(ctx, c.NATS, accountID, 1); err != nil {
			return nil, err
		}
		return ec2eipapi.AllocateAddress(ctx, input, c.NATS, accountID)
	}),
	"ReleaseAddress": ec2Handler(func(ctx context.Context, input *ec2.ReleaseAddressInput, c *call, accountID string) (any, error) {
		return ec2eipapi.ReleaseAddress(ctx, input, c.NATS, accountID)
	}),
	"AssociateAddress": ec2Handler(func(ctx context.Context, input *ec2.AssociateAddressInput, c *call, accountID string) (any, error) {
		return ec2eipapi.AssociateAddress(ctx, input, c.NATS, accountID)
	}),
	"DisassociateAddress": ec2Handler(func(ctx context.Context, input *ec2.DisassociateAddressInput, c *call, accountID string) (any, error) {
		return ec2eipapi.DisassociateAddress(ctx, input, c.NATS, accountID)
	}),
	"DescribeAddresses": ec2Handler(func(ctx context.Context, input *ec2.DescribeAddressesInput, c *call, accountID string) (any, error) {
		return ec2eipapi.DescribeAddresses(ctx, input, c.NATS, accountID)
	}),
	"DescribeAddressesAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeAddressesAttributeInput, c *call, accountID string) (any, error) {
		return ec2eipapi.DescribeAddressesAttribute(ctx, input, c.NATS, accountID)
	}),
	"CreateNatGateway": ec2IdempotentHandler(func(ctx context.Context, input *ec2.CreateNatGatewayInput, c *call, accountID string) (ec2.CreateNatGatewayOutput, error) {
		return ec2natgwapi.CreateNatGateway(ctx, input, c.NATS, accountID)
	}),
	"DeleteNatGateway": ec2Handler(func(ctx context.Context, input *ec2.DeleteNatGatewayInput, c *call, accountID string) (any, error) {
		return ec2natgwapi.DeleteNatGateway(ctx, input, c.NATS, accountID)
	}),
	"DescribeNatGateways": ec2Handler(func(ctx context.Context, input *ec2.DescribeNatGatewaysInput, c *call, accountID string) (any, error) {
		return ec2natgwapi.DescribeNatGateways(ctx, input, c.NATS, accountID)
	}),
}

// defaultVPCID returns the ID of accountID's default VPC, or "" when it has
// none, as the account's own DescribeVpcs sees it.
func defaultVPCID(ctx context.Context, natsConn *nats.Conn, accountID string) (string, error) {
	if natsConn == nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	out, err := ec2vpcapi.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{{Name: aws.String("is-default"), Values: aws.StringSlice([]string{"true"})}},
	}, natsConn, accountID)
	if err != nil {
		return "", err
	}
	if len(out.Vpcs) == 0 {
		return "", nil
	}
	return aws.StringValue(out.Vpcs[0].VpcId), nil
}

// ec2LocalActions are actions that don't require NATS. DescribeAccountAttributes
// needs it only for default-vpc.
var ec2LocalActions = map[string]bool{
	"DescribeRegions":           true,
	"DescribeAvailabilityZones": true,
	"DescribeAccountAttributes": true,
}

// dryRunRequested reports whether a parsed EC2 input carries DryRun=true. Only
// actions whose SDK input models the parameter can answer DryRunOperation.
func dryRunRequested(input any) bool {
	v := reflect.ValueOf(input)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return false
	}
	field := v.Elem().FieldByName("DryRun")
	if !field.IsValid() {
		return false
	}
	dryRun, ok := reflect.TypeAssert[*bool](field)
	return ok && aws.BoolValue(dryRun)
}
