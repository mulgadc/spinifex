package gateway

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// taggableTypes lists the TagSpecifications resource types an action accepts,
// and the code AWS refuses any other type with.
type taggableTypes struct {
	types []string
	code  string
}

func taggable(types ...string) taggableTypes {
	return taggableTypes{types: types, code: awserrors.ErrorInvalidParameterValue}
}

// ec2TaggableTypes covers every dispatched action whose input carries
// TagSpecifications; TestEC2TaggableTypes_CoverEveryAction keeps it complete.
// AWS answers some actions with InvalidParameter rather than InvalidParameterValue.
var ec2TaggableTypes = map[string]taggableTypes{
	"AllocateAddress":                 taggable("elastic-ip"),
	"AuthorizeSecurityGroupEgress":    {types: []string{"security-group-rule"}, code: awserrors.ErrorInvalidParameter},
	"AuthorizeSecurityGroupIngress":   {types: []string{"security-group-rule"}, code: awserrors.ErrorInvalidParameter},
	"CopyImage":                       taggable("image", "snapshot"),
	"CopySnapshot":                    taggable("snapshot"),
	"CreateCapacityReservation":       taggable("capacity-reservation"),
	"CreateEgressOnlyInternetGateway": taggable("egress-only-internet-gateway"),
	"CreateImage":                     taggable("image", "snapshot"),
	"CreateInternetGateway":           taggable("internet-gateway"),
	"CreateKeyPair":                   taggable("key-pair"),
	"CreateLaunchTemplate":            taggable("launch-template"),
	"CreateNatGateway":                taggable("natgateway"),
	"CreateNetworkInterface":          {types: []string{"network-interface"}, code: awserrors.ErrorInvalidParameter},
	"CreatePlacementGroup":            taggable("placement-group"),
	"CreateRouteTable":                taggable("route-table"),
	"CreateSecurityGroup":             taggable("security-group"),
	"CreateSnapshot":                  taggable("snapshot"),
	"CreateSubnet":                    {types: []string{"subnet"}, code: awserrors.ErrorInvalidParameter},
	"CreateVolume":                    taggable("volume"),
	"CreateVpc":                       {types: []string{"vpc"}, code: awserrors.ErrorInvalidParameter},
	"ImportKeyPair":                   taggable("key-pair"),
	"RegisterImage":                   taggable("image"),
	"RequestSpotInstances":            taggable("spot-instances-request"),
	"RunInstances":                    taggable("instance", "volume", "network-interface", "spot-instances-request"),
}

// validateTagSpecificationTypes refuses a TagSpecifications entry whose
// ResourceType the action cannot tag, rather than silently dropping its tags.
func validateTagSpecificationTypes(action string, input any) error {
	allowed, ok := ec2TaggableTypes[action]
	if !ok {
		return nil
	}
	for _, spec := range tagSpecifications(input) {
		if spec == nil || spec.ResourceType == nil {
			continue
		}
		if rt := aws.StringValue(spec.ResourceType); !slices.Contains(allowed.types, rt) {
			return awserrors.Errorf(allowed.code, "'%s' is not a valid taggable resource type for this operation.", rt)
		}
	}
	return nil
}

// tagSpecifications reads an EC2 input's TagSpecifications, or nil if it has none.
func tagSpecifications(input any) []*ec2.TagSpecification {
	v := reflect.ValueOf(input)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return nil
	}
	field := v.Elem().FieldByName("TagSpecifications")
	if !field.IsValid() {
		return nil
	}
	specs, _ := reflect.TypeAssert[[]*ec2.TagSpecification](field)
	return specs
}

// maxResultsRange is the MaxResults range one action enforces, and the code and
// text it refuses a value outside it with; AWS words each action differently.
type maxResultsRange struct {
	min, max int64
	code     string
	message  func(n int64) string
}

func natGatewaysMaxResultsMessage(n int64) string {
	bound := "less than or equal to 1000"
	if n < 5 {
		bound = "greater than or equal to 5"
	}
	return fmt.Sprintf("1 validation error detected: Value '%d' at 'maxResults' failed to satisfy constraint: Member must have value %s", n, bound)
}

var ec2MaxResultsRanges = map[string]maxResultsRange{
	"DescribeCapacityReservations": {1, 1000, awserrors.ErrorInvalidParameterValue, func(n int64) string {
		return fmt.Sprintf("%d is an invalid value for MaxResults. MaxResults must be null, or between 1 and 1000.", n)
	}},
	"DescribeEgressOnlyInternetGateways": {5, 255, awserrors.ErrorInvalidParameterValue, func(n int64) string {
		if n < 5 {
			return fmt.Sprintf("Value (%d) for parameter maxResults is invalid. Expecting a value greater than 5.", n)
		}
		return fmt.Sprintf("Value (%d) for parameter maxResults is invalid. Expecting a value less than 255.", n)
	}},
	"DescribeInstanceCreditSpecifications": {5, 1000, awserrors.ErrorInvalidRequest, func(n int64) string {
		return fmt.Sprintf("The value %d for the maxResults parameter must be between 5 and 1000. Change the value and try again.", n)
	}},
	"DescribeInstanceTypeOfferings": {5, 1000, awserrors.ErrorInvalidMaxResults, func(n int64) string {
		return fmt.Sprintf("Value ( %d ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 1000 ).", n)
	}},
	"DescribeLaunchTemplates": {1, 200, awserrors.ErrorInvalidParameterValue, func(int64) string {
		return "Maximum results allowed are between 1 and 200"
	}},
	"DescribeNatGateways": {5, 1000, awserrors.ErrorInvalidParameter, natGatewaysMaxResultsMessage},
}

// validateMaxResults refuses a MaxResults outside the action's range. It does
// not page; an in-range value is still answered with every result.
func validateMaxResults(action string, input any) error {
	r, ok := ec2MaxResultsRanges[action]
	if !ok {
		return nil
	}
	n, ok := int64Field(input, "MaxResults")
	if !ok || (n >= r.min && n <= r.max) {
		return nil
	}
	return awserrors.Errorf(r.code, "%s", r.message(n))
}

// int64Field reads a set *int64 field from an EC2 input.
func int64Field(input any, name string) (int64, bool) {
	v := reflect.ValueOf(input)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return 0, false
	}
	field := v.Elem().FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	p, ok := reflect.TypeAssert[*int64](field)
	if !ok || p == nil {
		return 0, false
	}
	return *p, true
}
