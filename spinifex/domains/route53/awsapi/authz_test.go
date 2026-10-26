package awsapi_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

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
