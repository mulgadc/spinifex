package route53_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	r53 "github.com/aws/aws-sdk-go/service/route53"
	nsconfig "github.com/mulgadc/northstar/pkg/config"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/route53"
)

// fakeNorthstar is the zone store plus a set of servers, each of which loads
// what the store holds only when it is not lagging.
type fakeNorthstar struct {
	mu      sync.Mutex
	objects map[string][]byte
	served  []map[string]string
	lagging map[int]bool
	puts    int
	deletes int
}

func newFakeNorthstar(servers int) *fakeNorthstar {
	f := &fakeNorthstar{objects: map[string][]byte{}, lagging: map[int]bool{}}
	for range servers {
		f.served = append(f.served, map[string]string{})
	}
	return f
}

func (f *fakeNorthstar) PutZone(_ context.Context, apex string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[apex] = body
	f.puts++
	return nil
}

func (f *fakeNorthstar) DeleteZone(_ context.Context, apex string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, apex)
	f.deletes++
	return nil
}

func (f *fakeNorthstar) Reload(_ context.Context, apex string) ([]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i, s := range f.served {
		if f.lagging[i] {
			continue
		}
		if body, ok := f.objects[apex]; ok {
			sum := md5.Sum(body)
			s[apex] = hex.EncodeToString(sum[:])
		} else {
			delete(s, apex)
		}
		out = append(out, s[apex])
	}
	return out, len(out) == len(f.served), nil
}

func (f *fakeNorthstar) zone(t *testing.T, apex string) nsconfig.ConfigArr {
	t.Helper()
	f.mu.Lock()
	body, ok := f.objects[apex]
	f.mu.Unlock()
	require.True(t, ok, "%s is not published", apex)
	var cfg nsconfig.ConfigArr
	require.NoError(t, toml.Unmarshal(body, &cfg))
	return cfg
}

func (f *fakeNorthstar) has(apex string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[apex]
	return ok
}

func setState(t *testing.T, svc *route53.Service, account, zoneID, state string) {
	t.Helper()
	require.NoError(t, svc.Store().MutateZone(ctx, account, strings.TrimPrefix(zoneID, "/hostedzone/"), func(z *route53.ZoneRecord) (bool, error) {
		z.State = state
		return true, nil
	}))
}

func changeStatus(t *testing.T, svc *route53.Service, account, changeID string) string {
	t.Helper()
	out, err := svc.GetChange(ctx, &r53.GetChangeInput{Id: aws.String(changeID)}, account)
	require.NoError(t, err)
	return aws.StringValue(out.ChangeInfo.Status)
}

func newRealizer(svc *route53.Service, ns *fakeNorthstar) *route53.Realizer {
	r := route53.NewRealizer(svc.Store(), ns)
	r.Reserved = svc.Reserved
	return r
}

func TestRealizerPublishesOnlyActiveZones(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(2)
	r := newRealizer(svc, ns)
	id := createZone(t, svc, acctA, "example.com")

	_, err := r.Reconcile(ctx)
	require.NoError(t, err)
	assert.False(t, ns.has("example.com"), "a zone pending its nameservers must not be served")

	setState(t, svc, acctA, id, route53.StateActive)
	out, err := apply(svc, acctA, id, change("CREATE", "www.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)
	assert.Equal(t, "PENDING", aws.StringValue(out.ChangeInfo.Status))

	revisit, err := r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Zero(t, revisit)
	zone := ns.zone(t, "example.com")
	assert.Equal(t, "example.com", zone.Domain.Domain)
	assert.Equal(t, "ns1.spx3.net.", zone.Domain.SOA)
	assert.Contains(t, zone.Records, nsconfig.Records{Domain: "www.", Type: 1, Class: 1, TTL: 60, Address: "192.0.2.1"})
	assert.Contains(t, zone.Records, nsconfig.Records{Domain: "", Type: 2, Class: 1, TTL: 172800, Address: "ns1.spx3.net."})
	assert.Equal(t, "INSYNC", changeStatus(t, svc, acctA, aws.StringValue(out.ChangeInfo.Id)))

	puts := ns.puts
	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, puts, ns.puts, "an unchanged zone is not rewritten")
}

// A change is INSYNC only once every northstar server serves it, so a server
// that has not loaded the new version holds it PENDING and asks for a retry.
func TestRealizerHoldsChangesPendingUntilEveryServerLoads(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(3)
	r := newRealizer(svc, ns)
	id := createZone(t, svc, acctA, "example.com")
	setState(t, svc, acctA, id, route53.StateActive)
	out, err := apply(svc, acctA, id, change("CREATE", "www.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)

	ns.lagging[2] = true
	revisit, err := r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Positive(t, revisit)
	assert.Equal(t, "PENDING", changeStatus(t, svc, acctA, aws.StringValue(out.ChangeInfo.Id)))

	ns.lagging[2] = false
	puts := ns.puts
	revisit, err = r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Zero(t, revisit)
	assert.Equal(t, puts, ns.puts, "the retry reloads without rewriting")
	assert.Equal(t, "INSYNC", changeStatus(t, svc, acctA, aws.StringValue(out.ChangeInfo.Id)))
}

func TestRealizerWithdrawsDeletedAndDeactivatedZones(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(2)
	r := newRealizer(svc, ns)
	gone := createZone(t, svc, acctA, "gone.com")
	lost := createZone(t, svc, acctA, "lost.com")
	setState(t, svc, acctA, gone, route53.StateActive)
	setState(t, svc, acctA, lost, route53.StateActive)
	_, err := r.Reconcile(ctx)
	require.NoError(t, err)
	require.True(t, ns.has("gone.com"))
	require.True(t, ns.has("lost.com"))

	del, err := svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: aws.String(gone)}, acctA)
	require.NoError(t, err)
	assert.Equal(t, "PENDING", aws.StringValue(del.ChangeInfo.Status))
	setState(t, svc, acctA, lost, route53.StateNameserversLost)

	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	assert.False(t, ns.has("gone.com"))
	assert.False(t, ns.has("lost.com"))
	assert.Equal(t, "INSYNC", changeStatus(t, svc, acctA, aws.StringValue(del.ChangeInfo.Id)))
	published, err := svc.Store().ListPublished(ctx)
	require.NoError(t, err)
	assert.Empty(t, published)
}

// Terraform polls GetChange after DeleteHostedZone, so a deletion must reach
// INSYNC even when no published entry is left to withdraw.
func TestRealizerSettlesTheDeletionOfAnUnservedZone(t *testing.T) {
	t.Run("never published", func(t *testing.T) {
		svc := newService(t)
		r := newRealizer(svc, newFakeNorthstar(1))
		id := createZone(t, svc, acctA, "early.com")
		setState(t, svc, acctA, id, route53.StateActive)

		del, err := svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: aws.String(id)}, acctA)
		require.NoError(t, err)
		require.Equal(t, "PENDING", aws.StringValue(del.ChangeInfo.Status))

		_, err = r.Reconcile(ctx)
		require.NoError(t, err)
		assert.Equal(t, "INSYNC", changeStatus(t, svc, acctA, aws.StringValue(del.ChangeInfo.Id)))
	})
	t.Run("withdrawn before the deletion was recorded", func(t *testing.T) {
		svc := newService(t)
		ns := newFakeNorthstar(1)
		r := newRealizer(svc, ns)
		id := createZone(t, svc, acctA, "racy.com")
		setState(t, svc, acctA, id, route53.StateActive)
		_, err := r.Reconcile(ctx)
		require.NoError(t, err)
		require.True(t, ns.has("racy.com"))

		del, err := svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: aws.String(id)}, acctA)
		require.NoError(t, err)
		// A pass that read the changes before the deletion's was written has
		// withdrawn the zone and forgotten it without marking the change.
		require.NoError(t, svc.Store().DeletePublished(ctx, "racy.com"))

		_, err = r.Reconcile(ctx)
		require.NoError(t, err)
		assert.Equal(t, "INSYNC", changeStatus(t, svc, acctA, aws.StringValue(del.ChangeInfo.Id)))
	})
}

func TestRealizerNeverWritesAReservedZone(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(1)
	r := newRealizer(svc, ns)
	id := createZone(t, svc, acctA, "example.com")
	setState(t, svc, acctA, id, route53.StateActive)
	r.Reserved = func(name string) bool { return name == "example.com." }

	_, err := r.Reconcile(ctx)
	require.NoError(t, err)
	assert.False(t, ns.has("example.com"))
}

// Two accounts may hold zones of the same name; if both are ACTIVE, the older
// one is served and the answer does not flip between them.
func TestRealizerServesTheOldestOfTwoActiveZones(t *testing.T) {
	svc := newService(t)
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	ns := newFakeNorthstar(1)
	r := newRealizer(svc, ns)
	first := createZone(t, svc, acctA, "example.com")
	second := createZone(t, svc, acctB, "example.com")
	_, err := apply(svc, acctB, second, change("CREATE", "www.example.com", "A", 60, "198.51.100.1"))
	require.NoError(t, err)
	setState(t, svc, acctB, second, route53.StateActive)
	setState(t, svc, acctA, first, route53.StateActive)

	for range 2 {
		_, err = r.Reconcile(ctx)
		require.NoError(t, err)
		for _, rec := range ns.zone(t, "example.com").Records {
			assert.NotEqual(t, "198.51.100.1", rec.Address, "the newer zone must not be served")
		}
	}
}

func TestRealizerDoesNothingWhenNotElected(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(1)
	r := newRealizer(svc, ns)
	r.Elect = func(context.Context) (func(), bool) { return nil, false }
	id := createZone(t, svc, acctA, "example.com")
	setState(t, svc, acctA, id, route53.StateActive)

	_, err := r.Reconcile(ctx)
	require.NoError(t, err)
	assert.False(t, ns.has("example.com"))
}

func TestRealizerRendersEveryRecordType(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(1)
	r := newRealizer(svc, ns)
	id := createZone(t, svc, acctA, "example.com")
	setState(t, svc, acctA, id, route53.StateActive)
	long := strings.Repeat("k", 255)
	_, err := apply(svc, acctA, id,
		change("CREATE", "example.com", "MX", 300, "10 mail.example.com."),
		change("CREATE", "_xmpp._tcp.example.com", "SRV", 300, "1 10 5269 xmpp.example.com."),
		change("CREATE", "example.com", "CAA", 300, `0 issue "letsencrypt.org"`),
		change("CREATE", "dkim.example.com", "TXT", 300, `"`+long+`" "tail"`, `"say \"hi\""`),
		change("CREATE", "*.example.com", "CNAME", 300, "www.example.com."),
		change("CREATE", "v6.example.com", "AAAA", 300, "2001:db8::1"),
	)
	require.NoError(t, err)

	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	records := ns.zone(t, "example.com").Records
	assert.Contains(t, records, nsconfig.Records{Domain: "", Type: 15, Class: 1, TTL: 300, Preference: 10, Address: "mail.example.com."})
	assert.Contains(t, records, nsconfig.Records{Domain: "_xmpp._tcp.", Type: 33, Class: 1, TTL: 300, Priority: 1, Weight: 10, Port: 5269, Address: "xmpp.example.com."})
	assert.Contains(t, records, nsconfig.Records{Domain: "", Type: 257, Class: 1, TTL: 300, CAATag: "issue", Address: "letsencrypt.org"})
	assert.Contains(t, records, nsconfig.Records{Domain: "dkim.", Type: 16, Class: 1, TTL: 300, Strings: []string{long, "tail"}})
	assert.Contains(t, records, nsconfig.Records{Domain: "dkim.", Type: 16, Class: 1, TTL: 300, Strings: []string{`say \"hi\"`}})
	assert.Contains(t, records, nsconfig.Records{Domain: "*.", Type: 5, Class: 1, TTL: 300, Address: "www.example.com."})
	assert.Contains(t, records, nsconfig.Records{Domain: "v6.", Type: 28, Class: 1, TTL: 300, Address: "2001:db8::1"})
}

// Route 53 accepts a target host without its trailing dot and serves it fully
// qualified; northstar refuses to pack a name that is not.
func TestRealizerQualifiesTargetsWithoutATrailingDot(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(1)
	r := newRealizer(svc, ns)
	id := createZone(t, svc, acctA, "example.com")
	setState(t, svc, acctA, id, route53.StateActive)
	_, err := apply(svc, acctA, id,
		change("CREATE", "example.com", "MX", 300, "10 mail.example.com"),
		change("CREATE", "_sip._tcp.example.com", "SRV", 300, "1 10 5060 sip.example.com"),
		change("CREATE", "www.example.com", "CNAME", 300, "web.example.net"),
		change("CREATE", "sub.example.com", "NS", 300, "ns1.example.net"),
	)
	require.NoError(t, err)

	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	records := ns.zone(t, "example.com").Records
	assert.Contains(t, records, nsconfig.Records{Domain: "", Type: 15, Class: 1, TTL: 300, Preference: 10, Address: "mail.example.com."})
	assert.Contains(t, records, nsconfig.Records{Domain: "_sip._tcp.", Type: 33, Class: 1, TTL: 300, Priority: 1, Weight: 10, Port: 5060, Address: "sip.example.com."})
	assert.Contains(t, records, nsconfig.Records{Domain: "www.", Type: 5, Class: 1, TTL: 300, Address: "web.example.net."})
	assert.Contains(t, records, nsconfig.Records{Domain: "sub.", Type: 2, Class: 1, TTL: 300, Address: "ns1.example.net."})

	out, err := svc.ListResourceRecordSets(ctx, &r53.ListResourceRecordSetsInput{HostedZoneId: &id, StartRecordName: new("example.com."), StartRecordType: new("MX")}, acctA)
	require.NoError(t, err)
	assert.Equal(t, "10 mail.example.com", *out.ResourceRecordSets[0].ResourceRecords[0].Value, "the stored value stays as entered, as in AWS")
}

// Route 53 serves the SOA record set as stored, so an edit to it is served too.
func TestRealizerPublishesTheStoredSOA(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(1)
	r := newRealizer(svc, ns)
	id := createZone(t, svc, acctA, "example.com")
	setState(t, svc, acctA, id, route53.StateActive)
	const soa = "ns1.spx3.net. ops.example.com. 7 3600 600 604800 300"
	_, err := apply(svc, acctA, id, change("UPSERT", "example.com", "SOA", 1200, soa))
	require.NoError(t, err)

	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	domain := ns.zone(t, "example.com").Domain
	assert.Equal(t, soa, domain.SOARecord)
	assert.Equal(t, uint32(1200), domain.SOATTL)
	assert.Equal(t, "ns1.spx3.net.", domain.SOA)
}

func TestTXTValuesAreValidatedAsCharacterStrings(t *testing.T) {
	svc := newService(t)
	id := createZone(t, svc, acctA, "example.com")
	for _, v := range []string{
		`"` + strings.Repeat("a", 256) + `"`,
		`"unterminated`,
		`"ok" trailing`,
		`"bad escape \999"`,
		strings.Repeat("b", 256),
	} {
		_, err := apply(svc, acctA, id, change("UPSERT", "t.example.com", "TXT", 60, v))
		requireCode(t, err, "InvalidChangeBatch")
	}
	for _, v := range []string{`"` + strings.Repeat("a", 255) + `"`, `"\255\"x"`, "unquoted", `"a" "b"`} {
		_, err := apply(svc, acctA, id, change("UPSERT", "t.example.com", "TXT", 60, v))
		require.NoError(t, err, v)
	}
}

func TestRealizerPrunesTheChangesOfDeletedZones(t *testing.T) {
	svc := newService(t)
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return clock }
	r := newRealizer(svc, newFakeNorthstar(1))
	r.Now = func() time.Time { return clock }
	id := createZone(t, svc, acctA, "example.com")
	del, err := svc.DeleteHostedZone(ctx, &r53.DeleteHostedZoneInput{Id: aws.String(id)}, acctA)
	require.NoError(t, err)
	changeID := aws.StringValue(del.ChangeInfo.Id)

	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, "INSYNC", changeStatus(t, svc, acctA, changeID), "kept while GetChange may still ask")

	clock = clock.Add(8 * 24 * time.Hour)
	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	_, err = svc.GetChange(ctx, &r53.GetChangeInput{Id: aws.String(changeID)}, acctA)
	requireCode(t, err, "NoSuchChange")
}

// The P0-2 scale, from the control plane's side: 1,100 active zones across
// three accounts are each published once, an idle pass writes nothing, and one
// changed zone is the only rewrite.
func TestRealizerPublishesEveryZoneAtScale(t *testing.T) {
	const zones = 1100
	svc := newService(t)
	ns := newFakeNorthstar(3)
	r := newRealizer(svc, ns)
	accounts := []string{acctA, acctB, "000000000013"}
	ids := make([]string, zones)
	for i := range zones {
		account := accounts[i%len(accounts)]
		ids[i] = createZone(t, svc, account, fmt.Sprintf("d%04d.example.com", i+1))
		setState(t, svc, account, ids[i], route53.StateActive)
	}

	revisit, err := r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Zero(t, revisit)
	assert.Equal(t, zones, ns.puts)
	for _, apex := range []string{"d0001.example.com", "d0550.example.com", "d1100.example.com"} {
		assert.True(t, ns.has(apex), "%s is not published", apex)
	}

	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, zones, ns.puts, "an idle pass rewrites nothing")

	_, err = apply(svc, accounts[1], ids[1], change("CREATE", "www.d0002.example.com", "A", 60, "192.0.2.2"))
	require.NoError(t, err)
	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, zones+1, ns.puts, "only the changed zone is rewritten")
	assert.Contains(t, ns.zone(t, "d0002.example.com").Records, nsconfig.Records{Domain: "www.", Type: 1, Class: 1, TTL: 60, Address: "192.0.2.2"})
}

// A change that leaves the rendered zone as it was, such as an UPSERT of the
// same value, still reaches INSYNC, or Terraform would wait on it forever.
func TestRealizerSyncsAChangeThatAltersNothing(t *testing.T) {
	svc := newService(t)
	ns := newFakeNorthstar(2)
	r := newRealizer(svc, ns)
	id := createZone(t, svc, acctA, "example.com")
	setState(t, svc, acctA, id, route53.StateActive)
	_, err := apply(svc, acctA, id, change("CREATE", "www.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)
	_, err = r.Reconcile(ctx)
	require.NoError(t, err)

	out, err := apply(svc, acctA, id, change("UPSERT", "www.example.com", "A", 60, "192.0.2.1"))
	require.NoError(t, err)
	puts := ns.puts
	_, err = r.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, puts, ns.puts, "identical content is not rewritten")
	assert.Equal(t, "INSYNC", changeStatus(t, svc, acctA, aws.StringValue(out.ChangeInfo.Id)))
}
