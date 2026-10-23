package awsapi_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	r53 "github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/domains/route53/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const account = "000000000011"

type authorized struct {
	mu    sync.Mutex
	calls []string
}

func (a *authorized) record(service, action string, resources []string, _ iampolicy.ConditionKeys) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, fmt.Sprintf("%s:%s %s", service, action, strings.Join(resources, ",")))
	return nil
}

// serveDomain answers route53.<Method> the way the daemon does, from the real
// service, so the SDK below exercises the whole wire path.
func serveDomain(t *testing.T, nc *nats.Conn, svc *route53.Service) {
	t.Helper()
	v := reflect.ValueOf(svc)
	for _, action := range awsapi.OperationInventory().Registered {
		m := v.MethodByName(action)
		require.True(t, m.IsValid(), "the service has no method for %s", action)
		inType := m.Type().In(1).Elem()
		sub, err := nc.Subscribe("route53."+action, func(msg *nats.Msg) {
			in := reflect.New(inType)
			require.NoError(t, json.Unmarshal(msg.Data, in.Interface()))
			out := m.Call([]reflect.Value{reflect.ValueOf(context.Background()), in, reflect.ValueOf(natsmsg.AccountIDFromMsg(msg))})
			if err, _ := reflect.TypeAssert[error](out[1]); err != nil {
				_, message, _ := awserrors.ResolveErrorDetail(err)
				_ = msg.Respond(awserrors.GenerateErrorPayloadWithMessage(awserrors.ValidErrorCodeFromError(err), message))
				return
			}
			body, err := json.Marshal(out[0].Interface())
			require.NoError(t, err)
			_ = msg.Respond(body)
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
}

// newClient returns an SDK client whose requests run through the
// registration, with errors rendered as the gateway renders them.
func newClient(t *testing.T) (*r53.Route53, *authorized) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	svc := route53.NewService(route53.NewStore(js))
	svc.Nameservers = func() []string { return []string{"ns1.spx3.net", "ns2.spx3.net"} }
	serveDomain(t, nc, svc)

	reg := awsapi.NewRegistration(awsapi.Deps{NATS: nc})
	auth := &authorized{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := reg.Dispatch(w, dispatch.Invocation{Request: r, AccountID: account, Region: "us-east-1", Authorize: auth.record})
		if err == nil {
			return
		}
		code, message, _ := awserrors.ResolveErrorDetail(err)
		if code == "" {
			code = err.Error()
		}
		meta := awserrors.LookupErrorMessage(awsapi.ServiceName, code)
		if message == "" {
			message = meta.Message
		}
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(meta.HTTPCode)
		_ = xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"ErrorResponse"`
			Type    string   `xml:"Error>Type"`
			Code    string   `xml:"Error>Code"`
			Message string   `xml:"Error>Message"`
		}{Type: "Sender", Code: code, Message: message})
	}))
	t.Cleanup(srv.Close)

	sess, err := session.NewSession(&aws.Config{
		Endpoint:    aws.String(srv.URL),
		Region:      aws.String("us-east-1"),
		Credentials: credentials.NewStaticCredentials("AKID", "secret", ""),
		MaxRetries:  aws.Int(0),
	})
	require.NoError(t, err)
	return r53.New(sess), auth
}

func requireAWSCode(t *testing.T, err error, code string) {
	t.Helper()
	var aerr awserr.Error
	require.ErrorAs(t, err, &aerr)
	assert.Equal(t, code, aerr.Code(), aerr.Message())
}

func TestSDKDrivesTheTerraformSequence(t *testing.T) {
	client, auth := newClient(t)

	created, err := client.CreateHostedZone(&r53.CreateHostedZoneInput{
		Name: aws.String("example.com"), CallerReference: aws.String("terraform-1"),
		HostedZoneConfig: &r53.HostedZoneConfig{Comment: aws.String("Managed by Terraform")},
	})
	require.NoError(t, err)
	zoneID := aws.StringValue(created.HostedZone.Id)
	bare := strings.TrimPrefix(zoneID, "/hostedzone/")
	assert.True(t, strings.HasSuffix(aws.StringValue(created.Location), "/2013-04-01/hostedzone/"+bare), aws.StringValue(created.Location))
	assert.Equal(t, []string{"ns1.spx3.net", "ns2.spx3.net"}, aws.StringValueSlice(created.DelegationSet.NameServers))
	assert.Equal(t, "INSYNC", aws.StringValue(created.ChangeInfo.Status))
	assert.False(t, created.ChangeInfo.SubmittedAt.IsZero())

	_, err = client.GetChange(&r53.GetChangeInput{Id: created.ChangeInfo.Id})
	require.NoError(t, err)

	got, err := client.GetHostedZone(&r53.GetHostedZoneInput{Id: aws.String(zoneID)})
	require.NoError(t, err)
	assert.Equal(t, "example.com.", aws.StringValue(got.HostedZone.Name))
	assert.Equal(t, "Managed by Terraform", aws.StringValue(got.HostedZone.Config.Comment))

	tags, err := client.ListTagsForResource(&r53.ListTagsForResourceInput{ResourceType: aws.String("hostedzone"), ResourceId: aws.String(bare)})
	require.NoError(t, err)
	assert.Equal(t, bare, aws.StringValue(tags.ResourceTagSet.ResourceId))

	record := &r53.ResourceRecordSet{
		Name: aws.String("www.example.com"), Type: aws.String("A"), TTL: aws.Int64(300),
		ResourceRecords: []*r53.ResourceRecord{{Value: aws.String("192.0.2.10")}},
	}
	changed, err := client.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(bare),
		ChangeBatch:  &r53.ChangeBatch{Comment: aws.String("Managed by Terraform"), Changes: []*r53.Change{{Action: aws.String("CREATE"), ResourceRecordSet: record}}},
	})
	require.NoError(t, err)
	assert.Equal(t, "Managed by Terraform", aws.StringValue(changed.ChangeInfo.Comment))

	sets, err := client.ListResourceRecordSets(&r53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(bare), MaxItems: aws.String("1"),
		StartRecordName: aws.String("www.example.com."), StartRecordType: aws.String("A"),
	})
	require.NoError(t, err)
	require.Len(t, sets.ResourceRecordSets, 1)
	assert.Equal(t, "www.example.com.", aws.StringValue(sets.ResourceRecordSets[0].Name))
	assert.Equal(t, "192.0.2.10", aws.StringValue(sets.ResourceRecordSets[0].ResourceRecords[0].Value))
	assert.Equal(t, int64(300), aws.Int64Value(sets.ResourceRecordSets[0].TTL))

	_, err = client.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(bare),
		ChangeBatch:  &r53.ChangeBatch{Changes: []*r53.Change{{Action: aws.String("CREATE"), ResourceRecordSet: record}}},
	})
	requireAWSCode(t, err, "InvalidChangeBatch")

	_, err = client.DeleteHostedZone(&r53.DeleteHostedZoneInput{Id: aws.String(bare)})
	requireAWSCode(t, err, "HostedZoneNotEmpty")

	record.Name = aws.String("www.example.com.")
	_, err = client.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(bare),
		ChangeBatch:  &r53.ChangeBatch{Comment: aws.String("Deleted by Terraform"), Changes: []*r53.Change{{Action: aws.String("DELETE"), ResourceRecordSet: record}}},
	})
	require.NoError(t, err)
	deleted, err := client.DeleteHostedZone(&r53.DeleteHostedZoneInput{Id: aws.String(bare)})
	require.NoError(t, err)
	_, err = client.GetChange(&r53.GetChangeInput{Id: deleted.ChangeInfo.Id})
	require.NoError(t, err)

	_, err = client.GetHostedZone(&r53.GetHostedZoneInput{Id: aws.String(bare)})
	requireAWSCode(t, err, "NoSuchHostedZone")

	assert.Contains(t, auth.calls, "route53:GetHostedZone arn:aws:route53:::hostedzone/"+bare)
	assert.Contains(t, auth.calls, "route53:ChangeResourceRecordSets arn:aws:route53:::hostedzone/"+bare)
	assert.Contains(t, auth.calls, "route53:ListTagsForResource arn:aws:route53:::hostedzone/"+bare)
	assert.Contains(t, auth.calls, "route53:CreateHostedZone *")
}

func TestSDKListsAndTags(t *testing.T) {
	client, _ := newClient(t)
	var ids []string
	for _, name := range []string{"b.example.com", "example.com", "example.org"} {
		out, err := client.CreateHostedZone(&r53.CreateHostedZoneInput{Name: aws.String(name), CallerReference: aws.String(name)})
		require.NoError(t, err)
		ids = append(ids, strings.TrimPrefix(aws.StringValue(out.HostedZone.Id), "/hostedzone/"))
	}

	var listed []string
	require.NoError(t, client.ListHostedZonesPages(&r53.ListHostedZonesInput{MaxItems: aws.String("2")}, func(p *r53.ListHostedZonesOutput, _ bool) bool {
		for _, z := range p.HostedZones {
			listed = append(listed, aws.StringValue(z.Name))
		}
		return true
	}))
	assert.ElementsMatch(t, []string{"b.example.com.", "example.com.", "example.org."}, listed)

	byName, err := client.ListHostedZonesByName(&r53.ListHostedZonesByNameInput{DNSName: aws.String("example.com"), MaxItems: aws.String("2")})
	require.NoError(t, err)
	require.Len(t, byName.HostedZones, 2)
	assert.Equal(t, "example.com.", aws.StringValue(byName.HostedZones[0].Name))
	assert.Equal(t, "b.example.com.", aws.StringValue(byName.HostedZones[1].Name))
	assert.True(t, aws.BoolValue(byName.IsTruncated))
	assert.Equal(t, "example.org.", aws.StringValue(byName.NextDNSName))

	count, err := client.GetHostedZoneCount(&r53.GetHostedZoneCountInput{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), aws.Int64Value(count.HostedZoneCount))

	_, err = client.UpdateHostedZoneComment(&r53.UpdateHostedZoneCommentInput{Id: aws.String(ids[0]), Comment: aws.String("moved")})
	require.NoError(t, err)

	_, err = client.ChangeTagsForResource(&r53.ChangeTagsForResourceInput{
		ResourceType: aws.String("hostedzone"), ResourceId: aws.String(ids[0]),
		AddTags: []*r53.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	})
	require.NoError(t, err)
	many, err := client.ListTagsForResources(&r53.ListTagsForResourcesInput{ResourceType: aws.String("hostedzone"), ResourceIds: aws.StringSlice(ids[:2])})
	require.NoError(t, err)
	require.Len(t, many.ResourceTagSets, 2)
	assert.Equal(t, "env", aws.StringValue(many.ResourceTagSets[0].Tags[0].Key))
}

func TestUnservedRouteIsInvalidAction(t *testing.T) {
	client, _ := newClient(t)
	_, err := client.CreateHealthCheck(&r53.CreateHealthCheckInput{
		CallerReference:   aws.String("hc"),
		HealthCheckConfig: &r53.HealthCheckConfig{Type: aws.String("HTTP"), FullyQualifiedDomainName: aws.String("example.com")},
	})
	requireAWSCode(t, err, "InvalidAction")
}

func TestOperationInventoryMatchesTheServiceMethods(t *testing.T) {
	inv := awsapi.OperationInventory()
	svc := reflect.TypeFor[*route53.Service]()
	for _, action := range inv.Registered {
		_, ok := svc.MethodByName(action)
		assert.True(t, ok, "%s is routed but the service does not implement it", action)
	}
	reg := awsapi.NewRegistration(awsapi.Deps{})
	assert.Equal(t, dispatch.ErrorEnvelopeXML, reg.Errors)
	assert.Equal(t, "route53", reg.Service)
}

func TestResolveActionMatchesEveryRoute(t *testing.T) {
	reg := awsapi.NewRegistration(awsapi.Deps{})
	cases := map[string]string{
		"POST /2013-04-01/hostedzone":                    "CreateHostedZone",
		"GET /2013-04-01/hostedzone":                     "ListHostedZones",
		"GET /2013-04-01/hostedzonesbyname":              "ListHostedZonesByName",
		"GET /2013-04-01/hostedzonecount":                "GetHostedZoneCount",
		"GET /2013-04-01/hostedzone/Z1":                  "GetHostedZone",
		"POST /2013-04-01/hostedzone/Z1":                 "UpdateHostedZoneComment",
		"DELETE /2013-04-01/hostedzone/Z1":               "DeleteHostedZone",
		"POST /2013-04-01/hostedzone/Z1/rrset/":          "ChangeResourceRecordSets",
		"GET /2013-04-01/hostedzone/Z1/rrset":            "ListResourceRecordSets",
		"GET /2013-04-01/change/C1":                      "GetChange",
		"POST /2013-04-01/tags/hostedzone/Z1":            "ChangeTagsForResource",
		"GET /2013-04-01/tags/hostedzone/Z1":             "ListTagsForResource",
		"POST /2013-04-01/tags/hostedzone":               "ListTagsForResources",
		"POST /2013-04-01/healthcheck":                   "",
		"GET /2013-04-01/hostedzone/Z1/rrset/extra/more": "",
	}
	for req, want := range cases {
		method, path, _ := strings.Cut(req, " ")
		r := httptest.NewRequest(method, path, nil)
		assert.Equal(t, want, reg.ResolveAction(r), req)
	}
	assert.Len(t, awsapi.OperationInventory().Registered, 13)
}
