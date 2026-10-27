package awsapi_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	route53sdk "github.com/aws/aws-sdk-go/service/route53"
	"github.com/stretchr/testify/assert"

	"github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/domains/route53/awsapi"
)

// accountWide are the actions AWS authorizes against "*", because they name no
// zone or change. Every other action must name the resource it acts on, or a
// policy scoped to one zone would grant it for every zone.
var accountWide = []string{"CreateHostedZone", "GetHostedZoneCount", "ListHostedZones", "ListHostedZonesByName"}

// Every served action, the extensions included, is authorized against the ARN
// AWS uses for it. A new action falls through to "*" unless it is mapped, which
// is what this test refuses.
func TestEveryActionIsAuthorizedAgainstItsResource(t *testing.T) {
	svc := reflect.TypeFor[*route53.Service]()
	for _, action := range append(awsapi.OperationInventory().Registered, awsapi.ExtensionActions()...) {
		t.Run(action, func(t *testing.T) {
			m, ok := svc.MethodByName(action)
			if !ok {
				t.Fatalf("the service has no method for %s", action)
			}
			in := reflect.New(m.Type.In(2).Elem())
			nameResources(in.Elem())
			arns := awsapi.ResourceARNs(in.Interface())

			if slices.Contains(accountWide, action) {
				assert.Equal(t, []string{"*"}, arns)
				return
			}
			assert.NotEmpty(t, arns)
			for _, arn := range arns {
				assert.True(t, strings.HasPrefix(arn, "arn:aws:route53:::") && strings.HasSuffix(arn, "/ZTEST"),
					"%s is authorized against %q, not the resource it names: map its input in authz.go", action, arn)
			}
		})
	}
}

// nameResources fills every field that identifies a resource, so the mapping
// has something to put in the ARN.
func nameResources(v reflect.Value) {
	for i := range v.NumField() {
		f, name := v.Field(i), v.Type().Field(i).Name
		switch {
		case !f.CanSet():
		case name == "ResourceType":
			f.Set(reflect.ValueOf(new("hostedzone")))
		case name == "Id" || name == "HostedZoneId" || name == "ResourceId":
			f.Set(reflect.ValueOf(new("ZTEST")))
		case name == "ResourceIds":
			f.Set(reflect.ValueOf([]*string{new("ZTEST")}))
		}
	}
}

// The daemon trims whitespace and the "/hostedzone/" or "/change/" prefix from
// an ID, so the ARN must name the same zone, or a Deny on it can be sidestepped.
func TestResourceARNsNameTheZoneTheDaemonActsOn(t *testing.T) {
	for _, id := range []string{"Z2", " Z2", "Z2\n", "/hostedzone/Z2", " /hostedzone/Z2 "} {
		for _, in := range []any{
			&route53sdk.GetHostedZoneInput{Id: &id},
			&route53sdk.ListResourceRecordSetsInput{HostedZoneId: &id},
			&route53sdk.ChangeTagsForResourceInput{ResourceType: new("hostedzone"), ResourceId: &id},
			&route53sdk.ListTagsForResourcesInput{ResourceType: new("hostedzone"), ResourceIds: []*string{&id}},
		} {
			assert.Equal(t, []string{"arn:aws:route53:::hostedzone/Z2"}, awsapi.ResourceARNs(in), "%T with ID %q", in, id)
		}
	}
	for _, id := range []string{"C2", " C2", "/change/C2"} {
		assert.Equal(t, []string{"arn:aws:route53:::change/C2"}, awsapi.ResourceARNs(&route53sdk.GetChangeInput{Id: &id}), "change ID %q", id)
	}
}
