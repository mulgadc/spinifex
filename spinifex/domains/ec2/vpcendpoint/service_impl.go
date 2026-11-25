package vpcendpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var _ VpcEndpointService = (*ServiceImpl)(nil)

// Settings are the formation facts the endpoint service renders.
type Settings struct {
	Region         string
	ServicesDomain string
	AZ             string
}

// ServiceImpl serves the endpoint actions from the endpoint bucket.
type ServiceImpl struct {
	*Store

	settings Settings
	// active admits work that needs every node to carry endpoint traffic, and
	// offered answers reads; until then the S3 endpoint service is not offered.
	// nil never admits.
	active  Admission
	offered func(context.Context) (bool, error)
}

// Admission reports whether every node can carry endpoint traffic. The caller
// holds done until its last write, and calls it on every path. A read failure
// returns an error and false.
type Admission func(ctx context.Context) (done func(), ok bool, err error)

// NewServiceImpl wraps an open store.
func NewServiceImpl(store *Store, settings Settings) *ServiceImpl {
	return &ServiceImpl{Store: store, settings: settings}
}

// NewServiceImplWithNATS opens the buckets and returns the service.
func NewServiceImplWithNATS(ctx context.Context, natsConn *nats.Conn, settings Settings) (*ServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	store, err := OpenStore(ctx, js)
	if err != nil {
		return nil, err
	}
	slog.Info("VPC endpoint service initialized", "bucket", KVBucketVpcEndpoints)
	return NewServiceImpl(store, settings), nil
}

// SetActivation installs the admission writes hold and the check reads use,
// which together offer the S3 endpoint service.
func (s *ServiceImpl) SetActivation(admit Admission, offered func(context.Context) (bool, error)) {
	s.active, s.offered = admit, offered
}

// serviceActive answers a read: whether the service is offered right now.
func (s *ServiceImpl) serviceActive(ctx context.Context) bool {
	if s.offered == nil {
		return false
	}
	ok, err := s.offered(ctx)
	if err != nil {
		slog.WarnContext(ctx, "VPC endpoint: feature level unreadable; service not offered", "err", err)
	}
	return ok && err == nil
}

// admit holds the service's admission for a write. Without one installed it
// admits nothing.
func (s *ServiceImpl) admit(ctx context.Context) (done func(), ok bool, err error) {
	if s.active == nil {
		return func() {}, false, nil
	}
	return s.active(ctx)
}

func (s *ServiceImpl) serviceName() string { return prefixlist.ServiceName(s.settings.Region) }

// internalError logs a store failure and answers it as AWS's server error.
func internalError(ctx context.Context, op string, err error) error {
	slog.ErrorContext(ctx, op+": store failure", "err", err)
	return errors.New(awserrors.ErrorServerInternal)
}

func endpointNotFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidVpcEndpointIdNotFound, "The Vpc Endpoint Id '%s' does not exist", id)
}

func unknownService(name string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidServiceName, "The Vpc Endpoint Service '%s' does not exist", name)
}

func (s *ServiceImpl) routeExists(table string) error {
	return awserrors.Errorf(awserrors.ErrorRouteAlreadyExists,
		"route table %s already has a route with destination-prefix-list-id %s", table, prefixlist.ID(s.settings.Region))
}

// checkTypeAndService applies AWS's type and service checks: a type outside
// AWS's enum, a service not offered here, then a type the service lacks.
func (s *ServiceImpl) checkTypeAndService(typ, name string) error {
	if typ == "" {
		typ = TypeGateway
	}
	switch typ {
	case TypeGateway, ec2.VpcEndpointTypeInterface, ec2.VpcEndpointTypeGatewayLoadBalancer:
	default:
		return awserrors.Errorf(awserrors.ErrorInvalidVpcEndpointType,
			"Unrecognized VPC endpoint type %s, expected one of [Gateway, Interface, GatewayLoadBalancer]", typ)
	}
	if name == "" {
		return awserrors.Errorf(awserrors.ErrorInvalidParameter, "service_name is a required member")
	}
	if name != s.serviceName() {
		// Another Region's S3 is an interface service on AWS.
		if (strings.HasPrefix(name, "com.amazonaws.") && strings.HasSuffix(name, ".s3")) ||
			(typ != ec2.VpcEndpointTypeInterface && s.interfaceOnly(name)) {
			return awserrors.Errorf(awserrors.ErrorInvalidParameter,
				"Endpoint type (%s) does not match available service types ([Interface]).", typ)
		}
		return unknownService(name)
	}
	if typ != TypeGateway {
		return awserrors.Errorf(awserrors.ErrorInvalidParameter,
			"Endpoint type (%s) does not match available service types ([Gateway]).", typ)
	}
	return nil
}

// interfaceOnlyServices are services Spinifex also serves that AWS offers in
// a Region only as interface endpoints, per AWS's PrivateLink service list.
var interfaceOnlyServices = []string{"ec2", "ecr.api", "ecr.dkr", "ecs", "eks", "elasticloadbalancing", "sts"}

// interfaceOnly reports whether name is one of interfaceOnlyServices in this Region.
func (s *ServiceImpl) interfaceOnly(name string) bool {
	svc, ok := strings.CutPrefix(name, "com.amazonaws."+s.settings.Region+".")
	return ok && slices.Contains(interfaceOnlyServices, svc)
}

// interfaceOnlyFields refuses the fields AWS accepts only on interface
// endpoints, in AWS's order. PrivateDnsEnabled=false is accepted, as on AWS.
func interfaceOnlyFields(subnets, groups, privateDNS, subnetConfigs bool) error {
	switch {
	case subnets:
		return awserrors.Errorf(awserrors.ErrorInvalidParameter, "Subnet IDs are only supported for Interface and GatewayLoadBalancer type VPC Endpoints.")
	case groups:
		return awserrors.Errorf(awserrors.ErrorInvalidParameter, "Security Group IDs are only supported for Interface type VPC Endpoint.")
	case privateDNS:
		return awserrors.Errorf(awserrors.ErrorInvalidParameter, "Private DNS is only supported for Interface type VPC Endpoint.")
	case subnetConfigs:
		return awserrors.Errorf(awserrors.ErrorInvalidParameter, "SubnetConfigurations are only supported for Interface type VPC Endpoint.")
	}
	return nil
}

// checkIPAddressType accepts ipv4 only: there is no IPv6 datapath.
func checkIPAddressType(name, value string) error {
	switch value {
	case "", ec2.IpAddressTypeIpv4:
		return nil
	case ec2.IpAddressTypeIpv6, ec2.IpAddressTypeDualstack:
		return awserrors.Errorf(awserrors.ErrorInvalidParameter,
			"%s %s is not supported: only ipv4 endpoints are available in this Region.", name, value)
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Invalid value '%s' for %s.", value, name)
}

// dnsRecordIPType returns the DnsRecordIpType to store, which AWS echoes.
func dnsRecordIPType(spec *ec2.DnsOptionsSpecification) (string, error) {
	if spec == nil || spec.DnsRecordIpType == nil {
		return "", nil
	}
	v := aws.StringValue(spec.DnsRecordIpType)
	if v == ec2.DnsRecordIpTypeServiceDefined {
		return "", nil
	}
	return v, checkIPAddressType("DnsRecordIpType", v)
}

// checkTables validates each named table against vpcID and returns them
// sorted and distinct.
func (s *Store) checkTables(ctx context.Context, accountID, vpcID string, ids []*string) ([]string, error) {
	tables := awsidentifiers.Distinct(ids)
	slices.Sort(tables)
	for _, t := range tables {
		vpc, err := s.tableVpc(ctx, accountID, t)
		if err != nil {
			return nil, err
		}
		if vpc == "" {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidRouteTableIdNotFound, "The Route Table Id '%s' does not exist.", t)
		}
		if vpc != vpcID {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameter, "%s is not in %s", t, vpcID)
		}
	}
	return tables, nil
}

// heldByOther returns the first of tables whose S3 route another endpoint
// holds, or "".
func (s *Store) heldByOther(ctx context.Context, tables []string, id string) (string, error) {
	for _, t := range tables {
		_, holder, err := s.claimHolder(ctx, t)
		if err != nil {
			return "", err
		}
		if holder != nil && holder.ID != id {
			return t, nil
		}
	}
	return "", nil
}

// overLimit reports whether one more endpoint would pass the account quota.
// Residue counts until swept, which caps the records every account read
// visits; concurrent creates may each pass the last free slot.
func (s *Store) overLimit(ctx context.Context, accountID string) (bool, error) {
	keys, err := listKeys(ctx, s.kv, accountID+".")
	if err != nil {
		return false, err
	}
	return len(keys) >= accountLimit, nil
}

// CreateVpcEndpoint creates a gateway endpoint for S3, available on return.
func (s *ServiceImpl) CreateVpcEndpoint(ctx context.Context, input *ec2.CreateVpcEndpointInput, accountID string) (*ec2.CreateVpcEndpointOutput, error) {
	vpcID := aws.StringValue(input.VpcId)
	if vpcID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameter, "vpc_id is a required member")
	}
	// Until every node can carry endpoint traffic the service does not exist,
	// as DescribeVpcEndpointServices shows. Admission is held to the last write.
	done, ok, err := s.admit(ctx)
	defer done()
	if err != nil {
		return nil, internalError(ctx, "CreateVpcEndpoint", err)
	}
	if !ok {
		return nil, unknownService(aws.StringValue(input.ServiceName))
	}
	vpc, err := s.vpcRecord(ctx, accountID, vpcID)
	if err != nil {
		return nil, internalError(ctx, "CreateVpcEndpoint", err)
	}
	if vpc == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidVpcIdNotFound, "The Vpc Id %s does not exist", vpcID)
	}
	if err := s.checkTypeAndService(aws.StringValue(input.VpcEndpointType), aws.StringValue(input.ServiceName)); err != nil {
		return nil, err
	}
	if err := interfaceOnlyFields(len(input.SubnetIds) > 0, len(input.SecurityGroupIds) > 0, aws.BoolValue(input.PrivateDnsEnabled), len(input.SubnetConfigurations) > 0); err != nil {
		return nil, err
	}
	if err := checkIPAddressType("IpAddressType", aws.StringValue(input.IpAddressType)); err != nil {
		return nil, err
	}
	dnsType, err := dnsRecordIPType(input.DnsOptions)
	if err != nil {
		return nil, err
	}
	tables, err := s.checkTables(ctx, accountID, vpcID, input.RouteTableIds)
	if err != nil {
		return nil, s.requestError(ctx, "CreateVpcEndpoint", err)
	}
	if held, err := s.heldByOther(ctx, tables, ""); err != nil {
		return nil, internalError(ctx, "CreateVpcEndpoint", err)
	} else if held != "" {
		return nil, s.routeExists(held)
	}
	var policy string
	if input.PolicyDocument != nil {
		if policy, err = checkPolicy(aws.StringValue(input.PolicyDocument)); err != nil {
			return nil, err
		}
	}
	if over, err := s.overLimit(ctx, accountID); err != nil {
		return nil, internalError(ctx, "CreateVpcEndpoint", err)
	} else if over {
		return nil, awserrors.Errorf(awserrors.ErrorVpcEndpointLimitExceeded, "The maximum number of VPC endpoints has been reached.")
	}

	rec := &EndpointRecord{
		ID:              awsidentifiers.GenerateResourceID("vpce"),
		AccountID:       accountID,
		VpcID:           vpcID,
		ServiceName:     s.serviceName(),
		Type:            TypeGateway,
		RouteTableIds:   tables,
		PolicyDocument:  policy,
		DnsRecordIpType: dnsType,
		Generation:      1,
		Tags:            awstags.Extract(input.TagSpecifications, ec2.ResourceTypeVpcEndpoint),
		CreatedAt:       s.now().UTC(),
	}
	if err := s.create(ctx, rec); err != nil {
		lost, isLost := errors.AsType[*claimLostError](err)
		gone := errors.Is(err, errVpcGone)
		if !isLost && !gone {
			return nil, internalError(ctx, "CreateVpcEndpoint", err)
		}
		slog.InfoContext(ctx, "CreateVpcEndpoint abandoned", "vpcEndpointId", rec.ID, "err", err)
		if gone {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidVpcIdNotFound, "The Vpc Id %s does not exist", vpcID)
		}
		return nil, s.routeExists(lost.table)
	}
	s.projectTags(ctx, accountID, rec.ID, rec.Tags)
	out := recordToEC2(rec, tables)
	if len(rec.Tags) == 0 {
		out.Tags = nil
	}
	return &ec2.CreateVpcEndpointOutput{VpcEndpoint: out, ClientToken: input.ClientToken}, nil
}

// requestError passes an AWS error through and answers anything else as an
// internal failure.
func (s *ServiceImpl) requestError(ctx context.Context, op string, err error) error {
	if _, ok := awserrors.ResolveErrorCode(err); ok {
		return err
	}
	return internalError(ctx, op, err)
}

// ModifyVpcEndpoint changes an endpoint's tables, policy or DNS options.
func (s *ServiceImpl) ModifyVpcEndpoint(ctx context.Context, input *ec2.ModifyVpcEndpointInput, accountID string) (*ec2.ModifyVpcEndpointOutput, error) {
	id := aws.StringValue(input.VpcEndpointId)
	rec, _, err := s.load(ctx, accountID, id)
	if err != nil {
		return nil, internalError(ctx, "ModifyVpcEndpoint", err)
	}
	if rec == nil || rec.State != StateAvailable {
		return nil, endpointNotFound(id)
	}
	subnets := len(input.AddSubnetIds) > 0 || len(input.RemoveSubnetIds) > 0
	groups := len(input.AddSecurityGroupIds) > 0 || len(input.RemoveSecurityGroupIds) > 0
	if err := interfaceOnlyFields(subnets, groups, aws.BoolValue(input.PrivateDnsEnabled), len(input.SubnetConfigurations) > 0); err != nil {
		return nil, err
	}
	if err := checkIPAddressType("IpAddressType", aws.StringValue(input.IpAddressType)); err != nil {
		return nil, err
	}
	m := modification{}
	if input.DnsOptions != nil {
		dnsType, err := dnsRecordIPType(input.DnsOptions)
		if err != nil {
			return nil, err
		}
		m.dns = &dnsType
	}
	reset := aws.BoolValue(input.ResetPolicy)
	switch {
	case input.PolicyDocument != nil && reset:
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameter, "Please specify either a reset flag or a policy document.")
	case input.PolicyDocument != nil:
		doc, err := checkPolicy(aws.StringValue(input.PolicyDocument))
		if err != nil {
			return nil, err
		}
		m.policy = &doc
	case reset:
		m.policy = aws.String("")
	}
	// A new route or a custom policy needs every node able to carry it; a
	// rollback in progress refuses either. Admission is held to the last write.
	if len(input.AddRouteTableIds) > 0 || (m.policy != nil && *m.policy != "") {
		done, ok, err := s.admit(ctx)
		defer done()
		if err != nil {
			return nil, internalError(ctx, "ModifyVpcEndpoint", err)
		}
		if !ok && len(input.AddRouteTableIds) > 0 {
			return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "Route tables cannot be added to VPC endpoints at the moment.")
		}
		if !ok {
			return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "Custom VPC endpoint policies cannot be set at the moment.")
		}
	}
	if m.add, err = s.checkTables(ctx, accountID, rec.VpcID, input.AddRouteTableIds); err != nil {
		return nil, s.requestError(ctx, "ModifyVpcEndpoint", err)
	}
	m.remove = awsidentifiers.Distinct(input.RemoveRouteTableIds)
	if held, err := s.heldByOther(ctx, m.add, id); err != nil {
		return nil, internalError(ctx, "ModifyVpcEndpoint", err)
	} else if held != "" {
		return nil, s.routeExists(held)
	}

	if _, err := s.modify(ctx, accountID, id, m); err != nil {
		lost, isLost := errors.AsType[*claimLostError](err)
		switch {
		case isLost:
			err = s.routeExists(lost.table)
		case errors.Is(err, errAbsent):
			err = endpointNotFound(id)
		case errors.Is(err, errNotAssociated):
			err = errors.New(awserrors.ErrorInvalidParameter)
		default:
			return nil, internalError(ctx, "ModifyVpcEndpoint", err)
		}
		slog.InfoContext(ctx, "ModifyVpcEndpoint refused", "vpcEndpointId", id, "err", err)
		return nil, err
	}
	return &ec2.ModifyVpcEndpointOutput{Return: aws.Bool(true)}, nil
}

// DeleteVpcEndpoints fences and removes each named endpoint with two writes,
// whatever its associations; what they leave is the sweep's. A store failure
// on one ID is that ID's, and the rest still complete.
func (s *ServiceImpl) DeleteVpcEndpoints(ctx context.Context, input *ec2.DeleteVpcEndpointsInput, accountID string) (*ec2.DeleteVpcEndpointsOutput, error) {
	ids := awsidentifiers.Distinct(input.VpcEndpointIds)
	if len(ids) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameter, "vpc_endpoint_id is a required member")
	}
	out := &ec2.DeleteVpcEndpointsOutput{Unsuccessful: []*ec2.UnsuccessfulItem{}}
	for _, id := range ids {
		found, err := s.remove(ctx, accountID, id)
		switch {
		case err != nil:
			slog.ErrorContext(ctx, "DeleteVpcEndpoints failed for one endpoint", "vpcEndpointId", id, "err", err)
			out.Unsuccessful = append(out.Unsuccessful, unsuccessful(id, awserrors.ErrorInternalError, "An internal error has occurred."))
		case !found:
			out.Unsuccessful = append(out.Unsuccessful, unsuccessful(id, awserrors.ErrorInvalidVpcEndpointNotFound, fmt.Sprintf("user %s does not own a resource %s", accountID, id)))
		}
	}
	return out, nil
}

func unsuccessful(id, code, msg string) *ec2.UnsuccessfulItem {
	return &ec2.UnsuccessfulItem{
		ResourceId: aws.String(id),
		Error:      &ec2.UnsuccessfulItemError{Code: aws.String(code), Message: aws.String(msg)},
	}
}
