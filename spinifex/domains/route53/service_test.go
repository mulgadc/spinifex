package route53_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	r53 "github.com/aws/aws-sdk-go/service/route53"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const (
	acctA = "000000000011"
	acctB = "000000000012"
)

var ctx = context.Background()

func newService(t *testing.T) *route53.Service {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	svc := route53.NewService(route53.NewStore(js))
	svc.Nameservers = func() []string { return []string{"ns1.spx3.net", "ns2.spx3.net"} }
	svc.Reserved = func(name string) bool { return name == "spx3.net." || strings.HasSuffix(name, ".compute.internal.") }
	return svc
}

func createZone(t *testing.T, svc *route53.Service, account, name string) string {
	t.Helper()
	out, err := svc.CreateHostedZone(ctx, &r53.CreateHostedZoneInput{
		Name: aws.String(name), CallerReference: aws.String("ref-" + name),
	}, account)
	require.NoError(t, err)
	return aws.StringValue(out.HostedZone.Id)
}

func requireCode(t *testing.T, err error, code string) string {
	t.Helper()
	require.Error(t, err)
	got, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "error carries no AWS code: %v", err)
	require.Equal(t, code, got, "message: %s", msg)
	return msg
}

func change(action, name, rrType string, ttl int64, values ...string) *r53.Change {
	set := &r53.ResourceRecordSet{Name: aws.String(name), Type: aws.String(rrType), TTL: aws.Int64(ttl)}
	for _, v := range values {
		set.ResourceRecords = append(set.ResourceRecords, &r53.ResourceRecord{Value: aws.String(v)})
	}
	return &r53.Change{Action: aws.String(action), ResourceRecordSet: set}
}

func apply(svc *route53.Service, account, zoneID string, changes ...*r53.Change) (*r53.ChangeResourceRecordSetsOutput, error) {
	return svc.ChangeResourceRecordSets(ctx, &r53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID), ChangeBatch: &r53.ChangeBatch{Changes: changes},
	}, account)
}

func listSets(t *testing.T, svc *route53.Service, account, zoneID string) []string {
	t.Helper()
	out, err := svc.ListResourceRecordSets(ctx, &r53.ListResourceRecordSetsInput{HostedZoneId: aws.String(zoneID)}, account)
	require.NoError(t, err)
	var got []string
	for _, s := range out.ResourceRecordSets {
		var vals []string
		for _, r := range s.ResourceRecords {
			vals = append(vals, aws.StringValue(r.Value))
		}
		got = append(got, fmt.Sprintf("%s %s %d %s", aws.StringValue(s.Name), aws.StringValue(s.Type), aws.Int64Value(s.TTL), strings.Join(vals, ",")))
	}
	return got
}

func TestCreateHostedZoneReturnsAWSShape(t *testing.T) {
	svc := newService(t)
	out, err := svc.CreateHostedZone(ctx, &r53.CreateHostedZoneInput{
		Name:             aws.String("Example.COM"),
		CallerReference:  aws.String("terraform-1"),
		HostedZoneConfig: &r53.HostedZoneConfig{Comment: aws.String("Managed by Terraform")},
	}, acctA)
	require.NoError(t, err)

	id := aws.StringValue(out.HostedZone.Id)
	assert.Regexp(t, `^/hostedzone/Z[A-Z0-9]{20}$`, id)
	assert.Equal(t, "example.com.", aws.StringValue(out.HostedZone.Name))
	assert.Equal(t, "Managed by Terraform", aws.StringValue(out.HostedZone.Config.Comment))
	assert.False(t, aws.BoolValue(out.HostedZone.Config.PrivateZone))
	assert.Equal(t, int64(2), aws.Int64Value(out.HostedZone.ResourceRecordSetCount))
	assert.Equal(t, []string{"ns1.spx3.net", "ns2.spx3.net"}, aws.StringValueSlice(out.DelegationSet.NameServers))
	assert.Equal(t, "/2013-04-01"+id, aws.StringValue(out.Location))
	assert.Regexp(t, `^/change/C[A-Z0-9]{20}$`, aws.StringValue(out.ChangeInfo.Id))
	assert.Equal(t, route53.ChangeStatusInSync, aws.StringValue(out.ChangeInfo.Status), "a zone not yet served is in sync once durable")

	assert.Equal(t, []string{
		"example.com. NS 172800 ns1.spx3.net.,ns2.spx3.net.",
		"example.com. SOA 900 ns1.spx3.net. hostmaster.spx3.net. 1 7200 900 1209600 86400",
	}, listSets(t, svc, acctA, id))

	got, err := svc.GetHostedZone(ctx, &r53.GetHostedZoneInput{Id: aws.String(strings.TrimPrefix(id, "/hostedzone/"))}, acctA)
	require.NoError(t, err)
	assert.Equal(t, id, aws.StringValue(got.HostedZone.Id), "a bare ID and a prefixed one name the same zone")
	assert.Equal(t, []string{"ns1.spx3.net", "ns2.spx3.net"}, aws.StringValueSlice(got.DelegationSet.NameServers))
}

func TestCreateHostedZoneRejections(t *testing.T) {
	svc := newService(t)
	createZone(t, svc, acctA, "example.com")

	cases := []struct {
		name string
		in   *r53.CreateHostedZoneInput
		code string
	}{
		{"repeated caller reference", &r53.CreateHostedZoneInput{Name: aws.String("other.com"), CallerReference: aws.String("ref-example.com")}, awserrors.ErrorHostedZoneAlreadyExists},
		{"bare TLD", &r53.CreateHostedZoneInput{Name: aws.String("com"), CallerReference: aws.String("r1")}, awserrors.ErrorInvalidDomainName},
		{"bad label", &r53.CreateHostedZoneInput{Name: aws.String("-bad.com"), CallerReference: aws.String("r2")}, awserrors.ErrorInvalidDomainName},
		{"platform zone", &r53.CreateHostedZoneInput{Name: aws.String("spx3.net"), CallerReference: aws.String("r3")}, awserrors.ErrorInvalidDomainName},
		{"private zone", &r53.CreateHostedZoneInput{Name: aws.String("p.com"), CallerReference: aws.String("r4"), VPC: &r53.VPC{VPCId: aws.String("vpc-1")}}, awserrors.ErrorInvalidInput},
		{"delegation set", &r53.CreateHostedZoneInput{Name: aws.String("d.com"), CallerReference: aws.String("r5"), DelegationSetId: aws.String("N1")}, awserrors.ErrorInvalidInput},
		{"no caller reference", &r53.CreateHostedZoneInput{Name: aws.String("n.com")}, awserrors.ErrorInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateHostedZone(ctx, tc.in, acctA)
			requireCode(t, err, tc.code)
		})
	}

	// The same reference in another account is a different request.
	_, err := svc.CreateHostedZone(ctx, &r53.CreateHostedZoneInput{Name: aws.String("example.com"), CallerReference: aws.String("ref-example.com")}, acctB)
	require.NoError(t, err)
}

func TestCreateHostedZoneEnforcesAccountLimit(t *testing.T) {
	svc := newService(t)
	svc.MaxZonesPerAccount = 2
	createZone(t, svc, acctA, "a.com")
	createZone(t, svc, acctA, "b.com")
	_, err := svc.CreateHostedZone(ctx, &r53.CreateHostedZoneInput{Name: aws.String("c.com"), CallerReference: aws.String("c")}, acctA)
	requireCode(t, err, awserrors.ErrorTooManyHostedZones)
	createZone(t, svc, acctB, "c.com")
}

func TestZonesAreInvisibleToOtherAccounts(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")

	_, err := svc.GetHostedZone(ctx, &r53.GetHostedZoneInput{Id: aws.String(id)}, acctB)
	requireCode(t, err, awserrors.ErrorNoSuchHostedZone)
	_, err = apply(svc, acctB, id, change("CREATE", "www.example.com", "A", 60, "192.0.2.1"))
	requireCode(t, err, awserrors.ErrorNoSuchHostedZone)
	_, err = svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: aws.String(id)}, acctB)
	requireCode(t, err, awserrors.ErrorNoSuchHostedZone)

	list, err := svc.ListHostedZones(ctx, &r53.ListHostedZonesInput{}, acctB)
	require.NoError(t, err)
	assert.Empty(t, list.HostedZones)
}

func TestChangeResourceRecordSetsCreateUpsertDelete(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")

	_, err := apply(svc, acctA, id,
		change("CREATE", "www.example.com", "A", 300, "192.0.2.10"),
		change("CREATE", "*.example.com", "TXT", 60, `"hello"`),
		change("CREATE", "example.com", "MX", 300, "10 mail.example.com"),
	)
	require.NoError(t, err)

	_, err = apply(svc, acctA, id, change("UPSERT", "WWW.example.com.", "A", 120, "192.0.2.11", "192.0.2.12"))
	require.NoError(t, err)

	assert.Equal(t, []string{
		"example.com. MX 300 10 mail.example.com",
		"example.com. NS 172800 ns1.spx3.net.,ns2.spx3.net.",
		"example.com. SOA 900 ns1.spx3.net. hostmaster.spx3.net. 1 7200 900 1209600 86400",
		`\052.example.com. TXT 60 "hello"`,
		"www.example.com. A 120 192.0.2.11,192.0.2.12",
	}, listSets(t, svc, acctA, id))

	msg := requireCode(t, func() error {
		_, err := apply(svc, acctA, id, change("DELETE", "www.example.com", "A", 120, "192.0.2.11"))
		return err
	}(), awserrors.ErrorInvalidChangeBatch)
	assert.Contains(t, msg, "Tried to delete resource record set [name='www.example.com.', type='A'] but the values provided do not match the current values")

	_, err = apply(svc, acctA, id, change("DELETE", "www.example.com.", "A", 120, "192.0.2.12", "192.0.2.11"))
	require.NoError(t, err, "a DELETE matches values regardless of order")
	assert.Len(t, listSets(t, svc, acctA, id), 4)
}

func TestChangeBatchIsAllOrNothing(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	_, err := apply(svc, acctA, id, change("CREATE", "www.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)

	msg := requireCode(t, func() error {
		_, err := apply(svc, acctA, id,
			change("CREATE", "api.example.com", "A", 60, "192.0.2.2"),
			change("CREATE", "www.example.com", "A", 60, "192.0.2.3"),
			change("DELETE", "gone.example.com", "A", 60, "192.0.2.4"),
		)
		return err
	}(), awserrors.ErrorInvalidChangeBatch)
	assert.Contains(t, msg, "Tried to create resource record set [name='www.example.com.', type='A'] but it already exists")
	assert.Contains(t, msg, "Tried to delete resource record set [name='gone.example.com.', type='A'] but it was not found")
	assert.Len(t, listSets(t, svc, acctA, id), 3, "the valid change in a failed batch must not land")

	_, err = apply(svc, acctA, id,
		change("DELETE", "www.example.com", "A", 60, "192.0.2.1"),
		change("CREATE", "www.example.com", "CNAME", 60, "lb.example.net"),
	)
	require.NoError(t, err, "changes apply in order, so a delete frees the name for a CNAME in the same batch")
}

func TestChangeBatchValidation(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	_, err := apply(svc, acctA, id, change("CREATE", "www.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)

	alias := change("CREATE", "lb.example.com", "A", 60)
	alias.ResourceRecordSet.TTL = nil
	alias.ResourceRecordSet.AliasTarget = &r53.AliasTarget{DNSName: aws.String("x.elb.spx3.net"), HostedZoneId: aws.String("Z1"), EvaluateTargetHealth: aws.Bool(false)}
	weighted := change("CREATE", "w.example.com", "A", 60, "192.0.2.1")
	weighted.ResourceRecordSet.SetIdentifier = aws.String("a")
	weighted.ResourceRecordSet.Weight = aws.Int64(10)

	cases := []struct {
		name   string
		change *r53.Change
		code   string
		msg    string
	}{
		{"CNAME beside A", change("CREATE", "www.example.com", "CNAME", 60, "x.example.net"), awserrors.ErrorInvalidChangeBatch, "conflicting RRSet of type A"},
		{"CNAME at apex", change("CREATE", "example.com", "CNAME", 60, "x.example.net"), awserrors.ErrorInvalidChangeBatch, "not permitted at apex"},
		{"name outside zone", change("CREATE", "www.example.org", "A", 60, "192.0.2.1"), awserrors.ErrorInvalidChangeBatch, "is not permitted in zone example.com."},
		{"apex NS delete", change("DELETE", "example.com", "NS", 172800, "ns1.spx3.net.", "ns2.spx3.net."), awserrors.ErrorInvalidChangeBatch, "must contain at least one NS record"},
		{"SOA below apex", change("CREATE", "sub.example.com", "SOA", 60, "a. b. 1 2 3 4 5"), awserrors.ErrorInvalidChangeBatch, "only permitted at the zone apex"},
		{"bad IPv4", change("CREATE", "v4.example.com", "A", 60, "2001:db8::1"), awserrors.ErrorInvalidChangeBatch, "not a valid IPv4 address"},
		{"bad IPv6", change("CREATE", "v6.example.com", "AAAA", 60, "192.0.2.1"), awserrors.ErrorInvalidChangeBatch, "not a valid IPv6 address"},
		{"bad MX", change("CREATE", "example.com", "MX", 60, "mail.example.com"), awserrors.ErrorInvalidChangeBatch, "priority host"},
		{"two CNAME values", change("CREATE", "c.example.com", "CNAME", 60, "a.example.net", "b.example.net"), awserrors.ErrorInvalidChangeBatch, "more than one value"},
		{"alias", alias, awserrors.ErrorInvalidInput, "alias"},
		{"weighted", weighted, awserrors.ErrorInvalidInput, "simple routing"},
		{"unsupported type", change("CREATE", "ds.example.com", "DS", 60, "1 2 3 abc"), awserrors.ErrorInvalidInput, "not supported"},
		{"negative TTL", change("CREATE", "t.example.com", "A", -1, "192.0.2.1"), awserrors.ErrorInvalidInput, "out of range"},
		{"no values", change("CREATE", "e.example.com", "A", 60), awserrors.ErrorInvalidInput, "at least one record"},
		{"wildcard below apex", change("CREATE", "*.sub.example.com", "A", 60, "192.0.2.1"), awserrors.ErrorInvalidInput, "directly below the zone apex"},
		{"malformed SOA", change("UPSERT", "example.com", "SOA", 900, "ns1.spx3.net. hostmaster.spx3.net. one 7200 900 1209600 86400"), awserrors.ErrorInvalidChangeBatch, "not a valid SOA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := apply(svc, acctA, id, tc.change)
			assert.Contains(t, requireCode(t, err, tc.code), tc.msg)
		})
	}

	_, err = apply(svc, acctA, id)
	requireCode(t, err, awserrors.ErrorInvalidInput)
}

func TestChangeBatchSizeLimits(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	values := make([]string, 0, 401)
	for i := range 401 {
		values = append(values, fmt.Sprintf("192.0.%d.%d", i/250, i%250+1))
	}
	_, err := apply(svc, acctA, id, change("CREATE", "many.example.com", "A", 60, values...))
	assert.Contains(t, requireCode(t, err, awserrors.ErrorInvalidChangeBatch), "more than 400 values")

	var batch []*r53.Change
	for i := range 3 {
		batch = append(batch, change("UPSERT", fmt.Sprintf("h%d.example.com", i), "A", 60, values[:200]...))
	}
	_, err = apply(svc, acctA, id, batch...)
	assert.Contains(t, requireCode(t, err, awserrors.ErrorInvalidChangeBatch), "Number of records limit of 1000 exceeded", "an UPSERT counts each record twice")
}

func TestConcurrentChangeBatchesBothLand(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Go(func() {
			_, errs[i] = apply(svc, acctA, id, change("CREATE", fmt.Sprintf("h%d.example.com", i), "A", 60, "192.0.2.1"))
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Len(t, listSets(t, svc, acctA, id), 4)
}

func TestListResourceRecordSetsPagesInRoute53Order(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	_, err := apply(svc, acctA, id,
		change("CREATE", "www.example.com", "A", 300, "192.0.2.10"),
		change("CREATE", "www.example.com", "AAAA", 300, "2001:db8::10"),
		change("CREATE", "a.b.example.com", "A", 300, "192.0.2.11"),
		change("CREATE", "z.example.com", "A", 300, "192.0.2.12"),
	)
	require.NoError(t, err)

	// The read Terraform makes for one aws_route53_record.
	out, err := svc.ListResourceRecordSets(ctx, &r53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(id), MaxItems: aws.String("1"),
		StartRecordName: aws.String("www.example.com."), StartRecordType: aws.String("A"),
	}, acctA)
	require.NoError(t, err)
	require.Len(t, out.ResourceRecordSets, 1)
	assert.Equal(t, "www.example.com.", aws.StringValue(out.ResourceRecordSets[0].Name))
	assert.Equal(t, "A", aws.StringValue(out.ResourceRecordSets[0].Type))
	assert.True(t, aws.BoolValue(out.IsTruncated))
	assert.Equal(t, "www.example.com.", aws.StringValue(out.NextRecordName))
	assert.Equal(t, "AAAA", aws.StringValue(out.NextRecordType))

	var names []string
	in := &r53.ListResourceRecordSetsInput{HostedZoneId: aws.String(id), MaxItems: aws.String("2")}
	for {
		page, err := svc.ListResourceRecordSets(ctx, in, acctA)
		require.NoError(t, err)
		for _, s := range page.ResourceRecordSets {
			names = append(names, aws.StringValue(s.Name)+" "+aws.StringValue(s.Type))
		}
		if !aws.BoolValue(page.IsTruncated) {
			break
		}
		in.StartRecordName, in.StartRecordType = page.NextRecordName, page.NextRecordType
	}
	assert.Equal(t, []string{
		"example.com. NS", "example.com. SOA", "a.b.example.com. A",
		"www.example.com. A", "www.example.com. AAAA", "z.example.com. A",
	}, names, "names sort by reversed label, so b.example.com sorts before www")

	_, err = svc.ListResourceRecordSets(ctx, &r53.ListResourceRecordSetsInput{HostedZoneId: aws.String(id), StartRecordType: aws.String("A")}, acctA)
	requireCode(t, err, awserrors.ErrorInvalidInput)
}

func TestDeleteHostedZoneRequiresOnlyDefaultRecords(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	_, err := apply(svc, acctA, id, change("CREATE", "sub.example.com", "NS", 300, "ns.elsewhere.net"))
	require.NoError(t, err)

	_, err = svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: aws.String(id)}, acctA)
	requireCode(t, err, awserrors.ErrorHostedZoneNotEmpty)

	_, err = apply(svc, acctA, id, change("DELETE", "sub.example.com", "NS", 300, "ns.elsewhere.net"))
	require.NoError(t, err)
	out, err := svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: aws.String(id)}, acctA)
	require.NoError(t, err)

	got, err := svc.GetChange(ctx, &r53.GetChangeInput{Id: out.ChangeInfo.Id}, acctA)
	require.NoError(t, err, "Terraform polls GetChange after DeleteHostedZone")
	assert.Equal(t, route53.ChangeStatusInSync, aws.StringValue(got.ChangeInfo.Status))

	_, err = svc.GetHostedZone(ctx, &r53.GetHostedZoneInput{Id: aws.String(id)}, acctA)
	requireCode(t, err, awserrors.ErrorNoSuchHostedZone)

	// The caller reference is released with the zone.
	_, err = svc.CreateHostedZone(ctx, &r53.CreateHostedZoneInput{Name: aws.String("example.com"), CallerReference: aws.String("ref-example.com")}, acctA)
	require.NoError(t, err)
}

func TestListHostedZonesPaging(t *testing.T) {
	svc := newService(t)
	for _, n := range []string{"b.example.com", "example.org", "a.example.com", "example.com"} {
		createZone(t, svc, acctA, n)
	}
	createZone(t, svc, acctB, "other.com")

	var ids []string
	in := &r53.ListHostedZonesInput{MaxItems: aws.String("3")}
	for {
		out, err := svc.ListHostedZones(ctx, in, acctA)
		require.NoError(t, err)
		for _, z := range out.HostedZones {
			ids = append(ids, aws.StringValue(z.Id))
		}
		if !aws.BoolValue(out.IsTruncated) {
			break
		}
		in.Marker = out.NextMarker
	}
	assert.Len(t, ids, 4)

	byName, err := svc.ListHostedZonesByName(ctx, &r53.ListHostedZonesByNameInput{}, acctA)
	require.NoError(t, err)
	var names []string
	for _, z := range byName.HostedZones {
		names = append(names, aws.StringValue(z.Name))
	}
	assert.Equal(t, []string{"example.com.", "a.example.com.", "b.example.com.", "example.org."}, names)

	from, err := svc.ListHostedZonesByName(ctx, &r53.ListHostedZonesByNameInput{DNSName: aws.String("b.example.com"), MaxItems: aws.String("1")}, acctA)
	require.NoError(t, err)
	require.Len(t, from.HostedZones, 1)
	assert.Equal(t, "b.example.com.", aws.StringValue(from.HostedZones[0].Name))
	assert.Equal(t, "example.org.", aws.StringValue(from.NextDNSName))

	count, err := svc.GetHostedZoneCount(ctx, &r53.GetHostedZoneCountInput{}, acctA)
	require.NoError(t, err)
	assert.Equal(t, int64(4), aws.Int64Value(count.HostedZoneCount))

	private, err := svc.ListHostedZones(ctx, &r53.ListHostedZonesInput{HostedZoneType: aws.String("PrivateHostedZone")}, acctA)
	require.NoError(t, err)
	assert.Empty(t, private.HostedZones)
}

func TestHostedZoneTags(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	bare := strings.TrimPrefix(id, "/hostedzone/")

	_, err := svc.ChangeTagsForResource(ctx, &r53.ChangeTagsForResourceInput{
		ResourceType: aws.String("hostedzone"), ResourceId: aws.String(bare),
		AddTags: []*r53.Tag{{Key: aws.String("env"), Value: aws.String("prod")}, {Key: aws.String("team"), Value: aws.String("dns")}},
	}, acctA)
	require.NoError(t, err)
	_, err = svc.ChangeTagsForResource(ctx, &r53.ChangeTagsForResourceInput{
		ResourceType: aws.String("hostedzone"), ResourceId: aws.String(bare), RemoveTagKeys: aws.StringSlice([]string{"team"}),
	}, acctA)
	require.NoError(t, err)

	out, err := svc.ListTagsForResource(ctx, &r53.ListTagsForResourceInput{ResourceType: aws.String("hostedzone"), ResourceId: aws.String(bare)}, acctA)
	require.NoError(t, err)
	assert.Equal(t, bare, aws.StringValue(out.ResourceTagSet.ResourceId))
	require.Len(t, out.ResourceTagSet.Tags, 1)
	assert.Equal(t, "env", aws.StringValue(out.ResourceTagSet.Tags[0].Key))

	many, err := svc.ListTagsForResources(ctx, &r53.ListTagsForResourcesInput{ResourceType: aws.String("hostedzone"), ResourceIds: aws.StringSlice([]string{bare})}, acctA)
	require.NoError(t, err)
	assert.Len(t, many.ResourceTagSets, 1)

	_, err = svc.ListTagsForResource(ctx, &r53.ListTagsForResourceInput{ResourceType: aws.String("hostedzone"), ResourceId: aws.String(bare)}, acctB)
	requireCode(t, err, awserrors.ErrorNoSuchHostedZone)
	_, err = svc.ListTagsForResource(ctx, &r53.ListTagsForResourceInput{ResourceType: aws.String("healthcheck"), ResourceId: aws.String("hc")}, acctA)
	requireCode(t, err, awserrors.ErrorRoute53NoSuchHealthCheck)

	tagged := func(keys ...string) *r53.ChangeTagsForResourceInput {
		in := &r53.ChangeTagsForResourceInput{ResourceType: aws.String("hostedzone"), ResourceId: aws.String(bare)}
		for _, k := range keys {
			in.AddTags = append(in.AddTags, &r53.Tag{Key: aws.String(k), Value: aws.String("v")})
		}
		return in
	}
	// One change carries at most 10 tags; with env, 49 more fill the zone.
	for first := 0; first < 49; first += 10 {
		var keys []string
		for i := first; i < min(first+10, 49); i++ {
			keys = append(keys, fmt.Sprintf("k%d", i))
		}
		_, err = svc.ChangeTagsForResource(ctx, tagged(keys...), acctA)
		require.NoError(t, err)
	}
	_, err = svc.ChangeTagsForResource(ctx, tagged("k49"), acctA)
	requireCode(t, err, awserrors.ErrorRoute53TooManyTagsForZone)
}

func TestChangeTagsForResourceBoundsItsLists(t *testing.T) {
	svc := newService(t)
	bare := strings.TrimPrefix(createZone(t, svc, acctA, "example.com"), "/hostedzone/")
	eleven := make([]*r53.Tag, 11)
	for i := range eleven {
		eleven[i] = &r53.Tag{Key: aws.String(fmt.Sprintf("k%d", i)), Value: aws.String("v")}
	}
	for name, in := range map[string]*r53.ChangeTagsForResourceInput{
		"empty AddTags":       {AddTags: []*r53.Tag{}},
		"11 AddTags":          {AddTags: eleven},
		"empty RemoveTagKeys": {RemoveTagKeys: []*string{}},
		"11 RemoveTagKeys":    {RemoveTagKeys: aws.StringSlice(strings.Split("a b c d e f g h i j k", " "))},
		"long RemoveTagKey":   {RemoveTagKeys: aws.StringSlice([]string{strings.Repeat("k", 129)})},
	} {
		in.ResourceType, in.ResourceId = aws.String("hostedzone"), aws.String(bare)
		_, err := svc.ChangeTagsForResource(ctx, in, acctA)
		require.Error(t, err, name)
		requireCode(t, err, awserrors.ErrorInvalidInput)
	}
}

func TestChangesOnAnActiveZoneWaitForSync(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	bare := strings.TrimPrefix(id, "/hostedzone/")
	require.NoError(t, svc.Store().MutateZone(ctx, acctA, bare, func(z *route53.ZoneRecord) (bool, error) {
		z.State = route53.StateActive
		return true, nil
	}))

	out, err := apply(svc, acctA, id, change("CREATE", "www.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)
	assert.Equal(t, route53.ChangeStatusPending, aws.StringValue(out.ChangeInfo.Status))

	doc, _, err := svc.Store().GetRRSets(ctx, acctA, bare)
	require.NoError(t, err)
	require.NoError(t, svc.Store().MarkChangesInSync(ctx, acctA, bare, doc.Version))
	got, err := svc.GetChange(ctx, &r53.GetChangeInput{Id: out.ChangeInfo.Id}, acctA)
	require.NoError(t, err)
	assert.Equal(t, route53.ChangeStatusInSync, aws.StringValue(got.ChangeInfo.Status))

	_, err = svc.GetChange(ctx, &r53.GetChangeInput{Id: out.ChangeInfo.Id}, acctB)
	requireCode(t, err, awserrors.ErrorNoSuchChange)
}

func TestUpdateHostedZoneComment(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	out, err := svc.UpdateHostedZoneComment(ctx, &r53.UpdateHostedZoneCommentInput{Id: aws.String(id), Comment: aws.String("moved")}, acctA)
	require.NoError(t, err)
	assert.Equal(t, "moved", aws.StringValue(out.HostedZone.Config.Comment))
	_, err = svc.UpdateHostedZoneComment(ctx, &r53.UpdateHostedZoneCommentInput{Id: aws.String(id), Comment: aws.String(strings.Repeat("x", 257))}, acctA)
	requireCode(t, err, awserrors.ErrorInvalidInput)
}

func TestChangeRecordsArePrunedAfterRetention(t *testing.T) {
	svc := newService(t)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return now }
	id := createZone(t, svc, acctA, "example.com")
	first, err := apply(svc, acctA, id, change("CREATE", "a.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)

	now = now.Add(8 * 24 * time.Hour)
	_, err = apply(svc, acctA, id, change("CREATE", "b.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)
	_, err = svc.GetChange(ctx, &r53.GetChangeInput{Id: first.ChangeInfo.Id}, acctA)
	requireCode(t, err, awserrors.ErrorNoSuchChange)
}

func TestLookupsOfUnissuableIDsNameNothing(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")

	// A key this long would exceed the NATS control line and close the
	// connection every later request shares.
	_, err := svc.GetChange(ctx, &r53.GetChangeInput{Id: aws.String(strings.Repeat("C", 6500))}, acctA)
	requireCode(t, err, awserrors.ErrorNoSuchChange)
	for _, bad := range []string{"Z1.*", "Z1 >"} {
		_, err = svc.GetHostedZone(ctx, &r53.GetHostedZoneInput{Id: aws.String(bad)}, acctA)
		requireCode(t, err, awserrors.ErrorNoSuchHostedZone)
	}

	_, err = svc.GetHostedZone(ctx, &r53.GetHostedZoneInput{Id: aws.String(id)}, acctA)
	require.NoError(t, err)
}

// AWS refuses a value outside the model's bounds with InvalidInput before it
// looks anything up, whatever the operation's semantic errors would be.
func TestModelBoundViolationsAreInvalidInput(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	longID := aws.String(strings.Repeat("Z", 33))
	longName := strings.Repeat("a", 1025)
	without := func(edit func(c *r53.Change)) *r53.Change {
		c := change("CREATE", "x.example.com", "TXT", 60, `"v"`)
		edit(c)
		return c
	}

	cases := map[string]func() error{
		"zone type outside enum": func() error {
			_, err := svc.ListHostedZones(ctx, &r53.ListHostedZonesInput{HostedZoneType: aws.String("PublicHostedZone")}, acctA)
			return err
		},
		"marker over 64": func() error {
			_, err := svc.ListHostedZones(ctx, &r53.ListHostedZonesInput{Marker: aws.String(strings.Repeat("Z", 65))}, acctA)
			return err
		},
		"by-name DNSName over 1024": func() error {
			_, err := svc.ListHostedZonesByName(ctx, &r53.ListHostedZonesByNameInput{DNSName: aws.String(longName)}, acctA)
			return err
		},
		"by-name zone ID over 32": func() error {
			_, err := svc.ListHostedZonesByName(ctx, &r53.ListHostedZonesByNameInput{DNSName: aws.String("example.com"), HostedZoneId: longID}, acctA)
			return err
		},
		"start name over 1024": func() error {
			_, err := svc.ListResourceRecordSets(ctx, &r53.ListResourceRecordSetsInput{HostedZoneId: aws.String(id), StartRecordName: aws.String(longName)}, acctA)
			return err
		},
		"start type outside enum": func() error {
			_, err := svc.ListResourceRecordSets(ctx, &r53.ListResourceRecordSetsInput{HostedZoneId: aws.String(id), StartRecordName: aws.String("example.com"), StartRecordType: aws.String("BOGUS")}, acctA)
			return err
		},
		"GetHostedZone ID over 32": func() error {
			_, err := svc.GetHostedZone(ctx, &r53.GetHostedZoneInput{Id: longID}, acctA)
			return err
		},
		"DeleteHostedZone ID over 32": func() error {
			_, err := svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: longID}, acctA)
			return err
		},
		"UpdateHostedZoneComment ID over 32": func() error {
			_, err := svc.UpdateHostedZoneComment(ctx, &r53.UpdateHostedZoneCommentInput{Id: longID, Comment: aws.String("c")}, acctA)
			return err
		},
		"ListResourceRecordSets ID over 32": func() error {
			_, err := svc.ListResourceRecordSets(ctx, &r53.ListResourceRecordSetsInput{HostedZoneId: longID}, acctA)
			return err
		},
		"ChangeResourceRecordSets ID over 32": func() error {
			_, err := apply(svc, acctA, *longID, change("CREATE", "x.example.com", "A", 60, "192.0.2.1"))
			return err
		},
		"action outside enum": func() error {
			_, err := apply(svc, acctA, id, change("REPLACE", "x.example.com", "A", 60, "192.0.2.1"))
			return err
		},
		"action missing": func() error {
			_, err := apply(svc, acctA, id, without(func(c *r53.Change) { c.Action = nil }))
			return err
		},
		"record name over 1024": func() error {
			_, err := apply(svc, acctA, id, without(func(c *r53.Change) { c.ResourceRecordSet.Name = aws.String(longName) }))
			return err
		},
		"record name missing": func() error {
			_, err := apply(svc, acctA, id, without(func(c *r53.Change) { c.ResourceRecordSet.Name = nil }))
			return err
		},
		"value over 4000": func() error {
			_, err := apply(svc, acctA, id, without(func(c *r53.Change) {
				c.ResourceRecordSet.ResourceRecords[0].Value = aws.String(strings.Repeat("v", 4001))
			}))
			return err
		},
		"value missing": func() error {
			_, err := apply(svc, acctA, id, without(func(c *r53.Change) { c.ResourceRecordSet.ResourceRecords[0].Value = nil }))
			return err
		},
		"zone name over 1024": func() error {
			_, err := svc.CreateHostedZone(ctx, &r53.CreateHostedZoneInput{Name: aws.String(longName), CallerReference: aws.String("long")}, acctA)
			return err
		},
		"zone name missing": func() error {
			_, err := svc.CreateHostedZone(ctx, &r53.CreateHostedZoneInput{CallerReference: aws.String("none")}, acctA)
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, requireCode(t, call(), awserrors.ErrorInvalidInput), "failed to satisfy constraint")
		})
	}
}
