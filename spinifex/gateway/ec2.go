// Package gateway provides the AWS-compatible API gateway for the Spinifex platform.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_account "github.com/mulgadc/spinifex/spinifex/gateway/ec2/account"
	gateway_ec2_capacityreservation "github.com/mulgadc/spinifex/spinifex/gateway/ec2/capacityreservation"
	gateway_ec2_eigw "github.com/mulgadc/spinifex/spinifex/gateway/ec2/eigw"
	gateway_ec2_eip "github.com/mulgadc/spinifex/spinifex/gateway/ec2/eip"
	gateway_ec2_igw "github.com/mulgadc/spinifex/spinifex/gateway/ec2/igw"
	gateway_ec2_image "github.com/mulgadc/spinifex/spinifex/gateway/ec2/image"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	gateway_ec2_key "github.com/mulgadc/spinifex/spinifex/gateway/ec2/key"
	gateway_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/gateway/ec2/launchtemplate"
	gateway_ec2_natgw "github.com/mulgadc/spinifex/spinifex/gateway/ec2/natgw"
	gateway_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/gateway/ec2/placementgroup"
	gateway_ec2_routetable "github.com/mulgadc/spinifex/spinifex/gateway/ec2/routetable"
	gateway_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/gateway/ec2/snapshot"
	gateway_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/spotinstance"
	gateway_ec2_tags "github.com/mulgadc/spinifex/spinifex/gateway/ec2/tags"
	gateway_ec2_volume "github.com/mulgadc/spinifex/spinifex/gateway/ec2/volume"
	gateway_ec2_vpc "github.com/mulgadc/spinifex/spinifex/gateway/ec2/vpc"
	gateway_ec2_zone "github.com/mulgadc/spinifex/spinifex/gateway/ec2/zone"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// EC2Handler processes parsed query args and returns XML response bytes.
// r is included for handlers that call gw.checkPolicyResource (e.g. iam:PassRole);
// most handlers ignore it.
type EC2Handler func(action string, q map[string]string, gw *GatewayConfig, accountID string, r *http.Request) ([]byte, error)

// requestContext returns r's context, or Background for a nil request
// (some callers and tests dispatch without an *http.Request).
func requestContext(r *http.Request) context.Context {
	if r == nil {
		return context.Background()
	}
	return r.Context()
}

// ec2Handler creates a type-safe EC2Handler: allocates the input struct,
// parses query params, calls the handler, and marshals output to XML.
func ec2Handler[In any](handler func(ctx context.Context, input *In, gw *GatewayConfig, accountID string) (any, error)) EC2Handler {
	return func(action string, q map[string]string, gw *GatewayConfig, accountID string, r *http.Request) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, err
		}
		output, err := handler(requestContext(r), input, gw, accountID)
		if err != nil {
			return nil, err
		}
		payload := utils.GenerateXMLPayload(action+"Response", output)
		xmlOutput, err := utils.MarshalToXML(payload)
		if err != nil {
			return nil, errors.New("failed to marshal response to XML")
		}
		return xmlOutput, nil
	}
}

// ec2HandlerWithReq is ec2Handler for actions that need the original *http.Request,
// e.g. RunInstances which enforces iam:PassRole on the supplied instance profile ARN.
func ec2HandlerWithReq[In any](handler func(ctx context.Context, input *In, gw *GatewayConfig, accountID string, r *http.Request) (any, error)) EC2Handler {
	return func(action string, q map[string]string, gw *GatewayConfig, accountID string, r *http.Request) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, err
		}
		output, err := handler(requestContext(r), input, gw, accountID, r)
		if err != nil {
			return nil, err
		}
		payload := utils.GenerateXMLPayload(action+"Response", output)
		xmlOutput, err := utils.MarshalToXML(payload)
		if err != nil {
			return nil, errors.New("failed to marshal response to XML")
		}
		return xmlOutput, nil
	}
}

var ec2Actions = map[string]EC2Handler{
	"DescribeInstances": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
		out, err := gateway_ec2_instance.DescribeInstances(ctx, input, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID)
		if err != nil {
			return out, err
		}
		gateway_ec2_instance.EnrichInstanceProfileIDs(out, gw.IAMService, accountID)
		return out, nil
	}),
	"RunInstances": ec2HandlerWithReq(func(ctx context.Context, input *ec2.RunInstancesInput, gw *GatewayConfig, accountID string, r *http.Request) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return gw.checkPolicyResource(r, "iam", "PassRole", roleARN)
		}
		launchQuotaCheck := func() error {
			return gw.Quota.EnforceLaunch(accountID, aws.StringValue(input.InstanceType), int(aws.Int64Value(input.MaxCount)))
		}
		reservation, err := gateway_ec2_instance.RunInstances(ctx, input, gw.NATSConn, gw.IAMService, accountID, passRoleCheck, launchQuotaCheck, gw.ExpectedNodes)
		if err != nil {
			return nil, err
		}
		// Charge the actual launched vCPUs; a counter write failure is drift for
		// reconcile to correct, so it must not fail the already-successful launch.
		if err := gw.Quota.ChargeLaunch(accountID, &reservation); err != nil {
			slog.WarnContext(ctx, "RunInstances: vcpu quota charge failed, reconcile will correct", "account", accountID, "err", err)
		}
		return reservation, nil
	}),
	"AssociateIamInstanceProfile": ec2HandlerWithReq(func(ctx context.Context, input *ec2.AssociateIamInstanceProfileInput, gw *GatewayConfig, accountID string, r *http.Request) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return gw.checkPolicyResource(r, "iam", "PassRole", roleARN)
		}
		return gateway_ec2_instance.AssociateIamInstanceProfile(ctx, input, gw.NATSConn, gw.IAMService, accountID, passRoleCheck)
	}),
	"DisassociateIamInstanceProfile": ec2Handler(func(ctx context.Context, input *ec2.DisassociateIamInstanceProfileInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.DisassociateIamInstanceProfile(ctx, input, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID)
	}),
	"ReplaceIamInstanceProfileAssociation": ec2HandlerWithReq(func(ctx context.Context, input *ec2.ReplaceIamInstanceProfileAssociationInput, gw *GatewayConfig, accountID string, r *http.Request) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return gw.checkPolicyResource(r, "iam", "PassRole", roleARN)
		}
		return gateway_ec2_instance.ReplaceIamInstanceProfileAssociation(ctx, input, gw.NATSConn, gw.IAMService, gw.DiscoverActiveNodes(ctx), accountID, passRoleCheck)
	}),
	"DescribeIamInstanceProfileAssociations": ec2Handler(func(ctx context.Context, input *ec2.DescribeIamInstanceProfileAssociationsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.DescribeIamInstanceProfileAssociations(ctx, input, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID)
	}),
	"StartInstances": ec2Handler(func(ctx context.Context, input *ec2.StartInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.StartInstances(ctx, input, gw.NATSConn, accountID)
	}),
	"StopInstances": ec2Handler(func(ctx context.Context, input *ec2.StopInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.StopInstances(ctx, input, gw.NATSConn, accountID)
	}),
	"RebootInstances": ec2Handler(func(ctx context.Context, input *ec2.RebootInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.RebootInstances(ctx, input, gw.NATSConn, accountID)
	}),
	"TerminateInstances": ec2Handler(func(ctx context.Context, input *ec2.TerminateInstancesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.TerminateInstances(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeInstanceTypes": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceTypesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.DescribeInstanceTypes(ctx, input, gw.NATSConn, gw.ExpectedNodes, accountID)
	}),
	"DescribeInstanceStatus": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceStatusInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.DescribeInstanceStatus(ctx, input, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID, gw.AZ)
	}),
	"GetConsoleOutput": ec2Handler(func(ctx context.Context, input *ec2.GetConsoleOutputInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.GetConsoleOutput(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyInstanceAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyInstanceAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		var delta int
		if input.InstanceType != nil {
			resolve := handlers_quota.NATSInstanceTypeResolver(gw.NATSConn, func() int { return gw.ExpectedNodes })
			d, err := gw.Quota.EnforceRetype(resolve, accountID, aws.StringValue(input.InstanceId), aws.StringValue(input.InstanceType.Value))
			if err != nil {
				return nil, err
			}
			delta = d
		}
		out, err := gateway_ec2_instance.ModifyInstanceAttribute(ctx, input, gw.NATSConn, accountID)
		if err != nil {
			return nil, err
		}
		// Charge the retype's vCPU growth; a counter write failure is drift for
		// reconcile to correct, so it must not fail the applied retype.
		if err := gw.Quota.AddVCPU(accountID, delta); err != nil {
			slog.WarnContext(ctx, "ModifyInstanceAttribute: vcpu quota charge failed, reconcile will correct", "account", accountID, "err", err)
		}
		return out, nil
	}),
	"DescribeInstanceAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.DescribeInstanceAttribute(ctx, input, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID)
	}),
	"ModifyInstanceMetadataOptions": ec2Handler(func(ctx context.Context, input *ec2.ModifyInstanceMetadataOptionsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.ModifyInstanceMetadataOptions(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeInstanceCreditSpecifications": ec2Handler(func(ctx context.Context, input *ec2.DescribeInstanceCreditSpecificationsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_instance.DescribeInstanceCreditSpecifications(input)
	}),
	"CreateKeyPair": ec2Handler(func(ctx context.Context, input *ec2.CreateKeyPairInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_key.CreateKeyPair(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteKeyPair": ec2Handler(func(ctx context.Context, input *ec2.DeleteKeyPairInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_key.DeleteKeyPair(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeKeyPairs": ec2Handler(func(ctx context.Context, input *ec2.DescribeKeyPairsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_key.DescribeKeyPairs(ctx, input, gw.NATSConn, accountID)
	}),
	"ImportKeyPair": func(action string, q map[string]string, gw *GatewayConfig, accountID string, r *http.Request) ([]byte, error) {
		// Parser leaves Base64 padding URL-encoded; decode it before dispatch.
		if strings.HasSuffix(q["PublicKeyMaterial"], "%3D%3D") {
			q["PublicKeyMaterial"] = strings.Replace(q["PublicKeyMaterial"], "%3D%3D", "==", 1)
		}
		return ec2Handler(func(ctx context.Context, input *ec2.ImportKeyPairInput, gw *GatewayConfig, accountID string) (any, error) {
			return gateway_ec2_key.ImportKeyPair(ctx, input, gw.NATSConn, accountID)
		})(action, q, gw, accountID, r)
	},
	"DescribeImages": ec2Handler(func(ctx context.Context, input *ec2.DescribeImagesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.DescribeImages(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateImage": ec2Handler(func(ctx context.Context, input *ec2.CreateImageInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.CreateImage(ctx, input, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID)
	}),
	"DeregisterImage": ec2Handler(func(ctx context.Context, input *ec2.DeregisterImageInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.DeregisterImage(ctx, input, gw.NATSConn, accountID)
	}),
	"RegisterImage": ec2Handler(func(ctx context.Context, input *ec2.RegisterImageInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.RegisterImage(ctx, input, gw.NATSConn, accountID)
	}),
	"CopyImage": ec2Handler(func(ctx context.Context, input *ec2.CopyImageInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.CopyImage(ctx, input, gw.NATSConn, gw.Region, accountID)
	}),
	"DescribeImageAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeImageAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.DescribeImageAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyImageAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyImageAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.ModifyImageAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"ResetImageAttribute": ec2Handler(func(ctx context.Context, input *ec2.ResetImageAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_image.ResetImageAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeRegions": ec2Handler(func(ctx context.Context, input *ec2.DescribeRegionsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_zone.DescribeRegions(input, gw.Region)
	}),
	"DescribeAvailabilityZones": ec2Handler(func(ctx context.Context, input *ec2.DescribeAvailabilityZonesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_zone.DescribeAvailabilityZones(input, gw.Region, gw.AZ)
	}),
	"DescribeVolumes": ec2Handler(func(ctx context.Context, input *ec2.DescribeVolumesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_volume.DescribeVolumes(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyVolume": ec2Handler(func(ctx context.Context, input *ec2.ModifyVolumeInput, gw *GatewayConfig, accountID string) (any, error) {
		if err := gw.Quota.EnforceVolumeModify(ctx, gw.NATSConn, accountID, aws.StringValue(input.VolumeId), int(aws.Int64Value(input.Size))); err != nil {
			return nil, err
		}
		return gateway_ec2_volume.ModifyVolume(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateVolume": ec2Handler(func(ctx context.Context, input *ec2.CreateVolumeInput, gw *GatewayConfig, accountID string) (any, error) {
		if err := gw.Quota.EnforceVolumeCreate(ctx, gw.NATSConn, accountID, int(aws.Int64Value(input.Size))); err != nil {
			return nil, err
		}
		return gateway_ec2_volume.CreateVolume(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteVolume": ec2Handler(func(ctx context.Context, input *ec2.DeleteVolumeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_volume.DeleteVolume(ctx, input, gw.NATSConn, gw.DiscoverActiveNodes(ctx), accountID)
	}),
	"AttachVolume": ec2Handler(func(ctx context.Context, input *ec2.AttachVolumeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_volume.AttachVolume(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeVolumeStatus": ec2Handler(func(ctx context.Context, input *ec2.DescribeVolumeStatusInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_volume.DescribeVolumeStatus(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeVolumesModifications": ec2Handler(func(ctx context.Context, input *ec2.DescribeVolumesModificationsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_volume.DescribeVolumesModifications(ctx, input, gw.NATSConn, accountID)
	}),
	"DetachVolume": ec2Handler(func(ctx context.Context, input *ec2.DetachVolumeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_volume.DetachVolume(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeAccountAttributes": ec2Handler(func(ctx context.Context, input *ec2.DescribeAccountAttributesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_account.DescribeAccountAttributes(input)
	}),
	"EnableEbsEncryptionByDefault": ec2Handler(func(ctx context.Context, input *ec2.EnableEbsEncryptionByDefaultInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_account.EnableEbsEncryptionByDefault(ctx, input, gw.NATSConn, accountID)
	}),
	"DisableEbsEncryptionByDefault": ec2Handler(func(ctx context.Context, input *ec2.DisableEbsEncryptionByDefaultInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_account.DisableEbsEncryptionByDefault(ctx, input, gw.NATSConn, accountID)
	}),
	"GetEbsEncryptionByDefault": ec2Handler(func(ctx context.Context, input *ec2.GetEbsEncryptionByDefaultInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_account.GetEbsEncryptionByDefault(ctx, input, gw.NATSConn, accountID)
	}),
	"GetSerialConsoleAccessStatus": ec2Handler(func(ctx context.Context, input *ec2.GetSerialConsoleAccessStatusInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_account.GetSerialConsoleAccessStatus(ctx, input, gw.NATSConn, accountID)
	}),
	"EnableSerialConsoleAccess": ec2Handler(func(ctx context.Context, input *ec2.EnableSerialConsoleAccessInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_account.EnableSerialConsoleAccess(ctx, input, gw.NATSConn, accountID)
	}),
	"DisableSerialConsoleAccess": ec2Handler(func(ctx context.Context, input *ec2.DisableSerialConsoleAccessInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_account.DisableSerialConsoleAccess(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateTags": ec2Handler(func(ctx context.Context, input *ec2.CreateTagsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_tags.CreateTags(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteTags": ec2Handler(func(ctx context.Context, input *ec2.DeleteTagsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_tags.DeleteTags(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeTags": ec2Handler(func(ctx context.Context, input *ec2.DescribeTagsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_tags.DescribeTags(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateSnapshot": ec2Handler(func(ctx context.Context, input *ec2.CreateSnapshotInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_snapshot.CreateSnapshot(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteSnapshot": ec2Handler(func(ctx context.Context, input *ec2.DeleteSnapshotInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_snapshot.DeleteSnapshot(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeSnapshots": ec2Handler(func(ctx context.Context, input *ec2.DescribeSnapshotsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_snapshot.DescribeSnapshots(ctx, input, gw.NATSConn, accountID)
	}),
	"CopySnapshot": ec2Handler(func(ctx context.Context, input *ec2.CopySnapshotInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_snapshot.CopySnapshot(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.CreateInternetGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_igw.CreateInternetGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.DeleteInternetGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_igw.DeleteInternetGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeInternetGateways": ec2Handler(func(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_igw.DescribeInternetGateways(ctx, input, gw.NATSConn, accountID)
	}),
	"AttachInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.AttachInternetGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_igw.AttachInternetGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"DetachInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.DetachInternetGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_igw.DetachInternetGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateEgressOnlyInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.CreateEgressOnlyInternetGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eigw.CreateEgressOnlyInternetGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteEgressOnlyInternetGateway": ec2Handler(func(ctx context.Context, input *ec2.DeleteEgressOnlyInternetGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eigw.DeleteEgressOnlyInternetGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeEgressOnlyInternetGateways": ec2Handler(func(ctx context.Context, input *ec2.DescribeEgressOnlyInternetGatewaysInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eigw.DescribeEgressOnlyInternetGateways(ctx, input, gw.NATSConn, accountID)
	}),
	"CreatePlacementGroup": ec2Handler(func(ctx context.Context, input *ec2.CreatePlacementGroupInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_placementgroup.CreatePlacementGroup(ctx, input, gw.NATSConn, accountID)
	}),
	"DeletePlacementGroup": ec2Handler(func(ctx context.Context, input *ec2.DeletePlacementGroupInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_placementgroup.DeletePlacementGroup(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribePlacementGroups": ec2Handler(func(ctx context.Context, input *ec2.DescribePlacementGroupsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_placementgroup.DescribePlacementGroups(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateCapacityReservation": ec2Handler(func(ctx context.Context, input *ec2.CreateCapacityReservationInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_capacityreservation.CreateCapacityReservation(ctx, input, gw.NATSConn, gw.ExpectedNodes, accountID)
	}),
	"DescribeCapacityReservations": ec2Handler(func(ctx context.Context, input *ec2.DescribeCapacityReservationsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_capacityreservation.DescribeCapacityReservations(ctx, input, gw.NATSConn, gw.ExpectedNodes, accountID)
	}),
	"CancelCapacityReservation": ec2Handler(func(ctx context.Context, input *ec2.CancelCapacityReservationInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_capacityreservation.CancelCapacityReservation(ctx, input, gw.NATSConn, gw.ExpectedNodes, accountID)
	}),
	"CreateLaunchTemplate": ec2Handler(func(ctx context.Context, input *ec2.CreateLaunchTemplateInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_launchtemplate.CreateLaunchTemplate(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateLaunchTemplateVersion": ec2Handler(func(ctx context.Context, input *ec2.CreateLaunchTemplateVersionInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_launchtemplate.CreateLaunchTemplateVersion(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteLaunchTemplate": ec2Handler(func(ctx context.Context, input *ec2.DeleteLaunchTemplateInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_launchtemplate.DeleteLaunchTemplate(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteLaunchTemplateVersions": ec2Handler(func(ctx context.Context, input *ec2.DeleteLaunchTemplateVersionsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_launchtemplate.DeleteLaunchTemplateVersions(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyLaunchTemplate": ec2Handler(func(ctx context.Context, input *ec2.ModifyLaunchTemplateInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_launchtemplate.ModifyLaunchTemplate(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeLaunchTemplates": ec2Handler(func(ctx context.Context, input *ec2.DescribeLaunchTemplatesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_launchtemplate.DescribeLaunchTemplates(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeLaunchTemplateVersions": ec2Handler(func(ctx context.Context, input *ec2.DescribeLaunchTemplateVersionsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_launchtemplate.DescribeLaunchTemplateVersions(ctx, input, gw.NATSConn, accountID)
	}),
	"RequestSpotInstances": ec2HandlerWithReq(func(ctx context.Context, input *ec2.RequestSpotInstancesInput, gw *GatewayConfig, accountID string, r *http.Request) (any, error) {
		passRoleCheck := func(roleARN string) error {
			return gw.checkPolicyResource(r, "iam", "PassRole", roleARN)
		}
		return gateway_ec2_spotinstance.RequestSpotInstances(ctx, input, gw.NATSConn, gw.IAMService, accountID, gw.AZ, passRoleCheck, gw.Quota, gw.ExpectedNodes)
	}),
	"DescribeSpotInstanceRequests": ec2Handler(func(ctx context.Context, input *ec2.DescribeSpotInstanceRequestsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_spotinstance.DescribeSpotInstanceRequests(ctx, input, gw.NATSConn, accountID)
	}),
	"CancelSpotInstanceRequests": ec2Handler(func(ctx context.Context, input *ec2.CancelSpotInstanceRequestsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_spotinstance.CancelSpotInstanceRequests(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateVpc": ec2Handler(func(ctx context.Context, input *ec2.CreateVpcInput, gw *GatewayConfig, accountID string) (any, error) {
		if err := gw.Quota.EnforceVPCs(ctx, gw.NATSConn, accountID, 1); err != nil {
			return nil, err
		}
		return gateway_ec2_vpc.CreateVpc(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteVpc": ec2Handler(func(ctx context.Context, input *ec2.DeleteVpcInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DeleteVpc(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeVpcs": ec2Handler(func(ctx context.Context, input *ec2.DescribeVpcsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DescribeVpcs(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyVpcAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyVpcAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.ModifyVpcAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeVpcAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeVpcAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DescribeVpcAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateSubnet": ec2Handler(func(ctx context.Context, input *ec2.CreateSubnetInput, gw *GatewayConfig, accountID string) (any, error) {
		if err := gw.Quota.EnforceSubnets(ctx, gw.NATSConn, accountID, 1); err != nil {
			return nil, err
		}
		return gateway_ec2_vpc.CreateSubnet(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteSubnet": ec2Handler(func(ctx context.Context, input *ec2.DeleteSubnetInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DeleteSubnet(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeSubnets": ec2Handler(func(ctx context.Context, input *ec2.DescribeSubnetsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DescribeSubnets(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifySubnetAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifySubnetAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.ModifySubnetAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateRouteTable": ec2Handler(func(ctx context.Context, input *ec2.CreateRouteTableInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.CreateRouteTable(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteRouteTable": ec2Handler(func(ctx context.Context, input *ec2.DeleteRouteTableInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.DeleteRouteTable(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeRouteTables": ec2Handler(func(ctx context.Context, input *ec2.DescribeRouteTablesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.DescribeRouteTables(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateRoute": ec2Handler(func(ctx context.Context, input *ec2.CreateRouteInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.CreateRoute(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteRoute": ec2Handler(func(ctx context.Context, input *ec2.DeleteRouteInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.DeleteRoute(ctx, input, gw.NATSConn, accountID)
	}),
	"ReplaceRoute": ec2Handler(func(ctx context.Context, input *ec2.ReplaceRouteInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.ReplaceRoute(ctx, input, gw.NATSConn, accountID)
	}),
	"AssociateRouteTable": ec2Handler(func(ctx context.Context, input *ec2.AssociateRouteTableInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.AssociateRouteTable(ctx, input, gw.NATSConn, accountID)
	}),
	"DisassociateRouteTable": ec2Handler(func(ctx context.Context, input *ec2.DisassociateRouteTableInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.DisassociateRouteTable(ctx, input, gw.NATSConn, accountID)
	}),
	"ReplaceRouteTableAssociation": ec2Handler(func(ctx context.Context, input *ec2.ReplaceRouteTableAssociationInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_routetable.ReplaceRouteTableAssociation(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateNetworkInterface": ec2Handler(func(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.CreateNetworkInterface(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteNetworkInterface": ec2Handler(func(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DeleteNetworkInterface(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeNetworkInterfaces": ec2Handler(func(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DescribeNetworkInterfaces(ctx, input, gw.NATSConn, accountID)
	}),
	"ModifyNetworkInterfaceAttribute": ec2Handler(func(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.ModifyNetworkInterfaceAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"AttachNetworkInterface": ec2Handler(func(ctx context.Context, input *ec2.AttachNetworkInterfaceInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.AttachNetworkInterface(ctx, input, gw.NATSConn, accountID)
	}),
	"DetachNetworkInterface": ec2Handler(func(ctx context.Context, input *ec2.DetachNetworkInterfaceInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DetachNetworkInterface(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateSecurityGroup": ec2Handler(func(ctx context.Context, input *ec2.CreateSecurityGroupInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.CreateSecurityGroup(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteSecurityGroup": ec2Handler(func(ctx context.Context, input *ec2.DeleteSecurityGroupInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DeleteSecurityGroup(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeSecurityGroups": ec2Handler(func(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DescribeSecurityGroups(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeSecurityGroupRules": ec2Handler(func(ctx context.Context, input *ec2.DescribeSecurityGroupRulesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.DescribeSecurityGroupRules(ctx, input, gw.NATSConn, accountID)
	}),
	"AuthorizeSecurityGroupIngress": ec2Handler(func(ctx context.Context, input *ec2.AuthorizeSecurityGroupIngressInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.AuthorizeSecurityGroupIngress(ctx, input, gw.NATSConn, accountID)
	}),
	"AuthorizeSecurityGroupEgress": ec2Handler(func(ctx context.Context, input *ec2.AuthorizeSecurityGroupEgressInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.AuthorizeSecurityGroupEgress(ctx, input, gw.NATSConn, accountID)
	}),
	"RevokeSecurityGroupIngress": ec2Handler(func(ctx context.Context, input *ec2.RevokeSecurityGroupIngressInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.RevokeSecurityGroupIngress(ctx, input, gw.NATSConn, accountID)
	}),
	"RevokeSecurityGroupEgress": ec2Handler(func(ctx context.Context, input *ec2.RevokeSecurityGroupEgressInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_vpc.RevokeSecurityGroupEgress(ctx, input, gw.NATSConn, accountID)
	}),
	"AllocateAddress": ec2Handler(func(ctx context.Context, input *ec2.AllocateAddressInput, gw *GatewayConfig, accountID string) (any, error) {
		if err := gw.Quota.EnforceEIPs(ctx, gw.NATSConn, accountID, 1); err != nil {
			return nil, err
		}
		return gateway_ec2_eip.AllocateAddress(ctx, input, gw.NATSConn, accountID)
	}),
	"ReleaseAddress": ec2Handler(func(ctx context.Context, input *ec2.ReleaseAddressInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eip.ReleaseAddress(ctx, input, gw.NATSConn, accountID)
	}),
	"AssociateAddress": ec2Handler(func(ctx context.Context, input *ec2.AssociateAddressInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eip.AssociateAddress(ctx, input, gw.NATSConn, accountID)
	}),
	"DisassociateAddress": ec2Handler(func(ctx context.Context, input *ec2.DisassociateAddressInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eip.DisassociateAddress(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeAddresses": ec2Handler(func(ctx context.Context, input *ec2.DescribeAddressesInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eip.DescribeAddresses(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeAddressesAttribute": ec2Handler(func(ctx context.Context, input *ec2.DescribeAddressesAttributeInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_eip.DescribeAddressesAttribute(ctx, input, gw.NATSConn, accountID)
	}),
	"CreateNatGateway": ec2Handler(func(ctx context.Context, input *ec2.CreateNatGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_natgw.CreateNatGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"DeleteNatGateway": ec2Handler(func(ctx context.Context, input *ec2.DeleteNatGatewayInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_natgw.DeleteNatGateway(ctx, input, gw.NATSConn, accountID)
	}),
	"DescribeNatGateways": ec2Handler(func(ctx context.Context, input *ec2.DescribeNatGatewaysInput, gw *GatewayConfig, accountID string) (any, error) {
		return gateway_ec2_natgw.DescribeNatGateways(ctx, input, gw.NATSConn, accountID)
	}),
}

// ec2LocalActions are actions that don't require NATS.
var ec2LocalActions = map[string]bool{
	"DescribeRegions":           true,
	"DescribeAvailabilityZones": true,
	"DescribeAccountAttributes": true,
}

func (gw *GatewayConfig) EC2_Request(w http.ResponseWriter, r *http.Request) error {
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.Debug("EC2: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	handler, ok := ec2Actions[action]
	if !ok {
		return errors.New(awserrors.ErrorInvalidAction)
	}

	if err := gw.checkPolicy(r, "ec2", action); err != nil {
		return err
	}

	if gw.NATSConn == nil && !ec2LocalActions[action] {
		return errors.New(awserrors.ErrorServerInternal)
	}

	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("EC2_Request: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	xmlOutput, err := handler(action, queryArgs, gw, accountID, r)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.Error("Failed to write EC2 response", "err", err)
	}
	return nil
}
