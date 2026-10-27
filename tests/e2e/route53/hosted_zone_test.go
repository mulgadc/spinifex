//go:build e2e

package route53

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	r53 "github.com/aws/aws-sdk-go/service/route53"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/route53/ext"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

const (
	wwwAddr = "192.0.2.80"
	apiAddr = "192.0.2.81"
	txtMark = "r53-e2e"
	mxPref  = 10

	// A check, a publish and a reload on every node, with headroom.
	activationTimeout = 3 * time.Minute
	syncTimeout       = 2 * time.Minute
)

// A customer's hosted zone is served only once its parent delegates it to the
// cluster, every node serves a change before it reports INSYNC, a second
// account cannot take the same name, and deleting the zone stops it resolving.
// The zone is a delegated subdomain of the cluster's own domain, so the real
// delegation check runs with no registrar and no internet.
func TestHostedZoneServedOnceDelegated(t *testing.T) {
	env := harness.LoadEnv(t)
	targets := harness.DNSTargets(env)
	base := harness.NorthstarBaseDomain(env)
	if base == "" && env.Mode == harness.ModeMultinode {
		base, _ = harness.PeerNorthstarDomains(t, targets[0])
	}
	require.NotEmpty(t, base, "northstar base domain is not configured")

	owner := newTenant(t, env, "r53-owner")
	c := owner.Client.Route53
	name := fmt.Sprintf("z%d.r53e2e.%s", time.Now().UnixNano()%1_000_000_000, base)
	www, api := "www."+name, "api."+name

	harness.Phase(t, "Route 53 — create %s", name)
	created, err := c.CreateHostedZone(&r53.CreateHostedZoneInput{Name: aws.String(name), CallerReference: aws.String(name)})
	require.NoError(t, err)
	zoneID := aws.StringValue(created.HostedZone.Id)
	t.Cleanup(func() { deleteZone(t, c, zoneID) })
	nameservers := aws.StringValueSlice(created.DelegationSet.NameServers)
	require.NotEmpty(t, nameservers)
	for _, ns := range nameservers {
		assert.True(t, strings.HasSuffix(ns, "."+base), "%s is not one of the cluster's name servers", ns)
	}
	waitInSync(t, c, created.ChangeInfo.Id)

	harness.Phase(t, "Route 53 — the base zone publishes the same name servers")
	for _, node := range targets {
		harness.EventuallyErr(t, func() error { return wantNS(node, base, nameservers) }, syncTimeout, 5*time.Second)
	}

	change, err := c.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &r53.ChangeBatch{Changes: []*r53.Change{
			rrset("CREATE", www, "A", wwwAddr),
			rrset("CREATE", name, "TXT", `"`+txtMark+`"`),
			rrset("CREATE", name, "MX", fmt.Sprintf("%d mail.%s.", mxPref, name)),
		}},
	})
	require.NoError(t, err)
	waitInSync(t, c, change.ChangeInfo.Id)

	harness.Phase(t, "Route 53 — an undelegated zone changes no answer")
	assert.Equal(t, "PENDING_NAMESERVERS", activation(t, c, zoneID, false).State)
	for _, node := range targets {
		require.NoError(t, wantGone(node, www), "an inactive zone must not be served")
	}

	harness.Phase(t, "Route 53 — delegate %s in %s and activate", name, base)
	harness.SpxRun(t, false, "admin", "dns", "delegate", name)
	t.Cleanup(func() { harness.SpxRunBestEffort(t, "admin", "dns", "delegate", "--remove", name) })
	harness.EventuallyErr(t, func() error {
		if a := activation(t, c, zoneID, true); a.State != "ACTIVE" {
			return fmt.Errorf("zone is %s: %s", a.State, a.Reason)
		}
		return nil
	}, activationTimeout, 5*time.Second)

	harness.Phase(t, "Route 53 — every node serves the zone")
	for _, node := range targets {
		harness.EventuallyErr(t, func() error { return wantAddr(node, www, wwwAddr) }, syncTimeout, 5*time.Second)
		require.NoError(t, wantTXT(node, name, txtMark))
		require.NoError(t, wantMX(node, name, "mail."+name+"."))
	}

	harness.Phase(t, "Route 53 — a change is INSYNC only once every node serves it")
	change, err = c.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch:  &r53.ChangeBatch{Changes: []*r53.Change{rrset("UPSERT", api, "A", apiAddr)}},
	})
	require.NoError(t, err)
	waitInSync(t, c, change.ChangeInfo.Id)
	for _, node := range targets {
		require.NoError(t, wantAddr(node, api, apiAddr), "INSYNC was reported before %s served the change", node.Name)
	}

	harness.Phase(t, "Route 53 — a second account cannot take an active name")
	rival := newTenant(t, env, "r53-rival").Client.Route53
	contested, err := rival.CreateHostedZone(&r53.CreateHostedZoneInput{Name: aws.String(name), CallerReference: aws.String("rival-" + name)})
	require.NoError(t, err)
	rivalID := aws.StringValue(contested.HostedZone.Id)
	t.Cleanup(func() { deleteZone(t, rival, rivalID) })
	a := activation(t, rival, rivalID, true)
	assert.Equal(t, "PENDING_NAMESERVERS", a.State)
	assert.Equal(t, "the domain is active in another account", a.Reason)
	_, err = rival.GetHostedZone(&r53.GetHostedZoneInput{Id: aws.String(zoneID)})
	require.Error(t, err, "one account must not see another's zone")

	harness.Phase(t, "Route 53 — a deleted zone stops resolving")
	deleted := deleteZone(t, c, zoneID)
	require.NotNil(t, deleted, "DeleteHostedZone must succeed")
	waitInSync(t, c, deleted)
	for _, node := range targets {
		harness.EventuallyErr(t, func() error { return wantGone(node, www) }, syncTimeout, 5*time.Second)
	}
}

func rrset(action, name, rrType, value string) *r53.Change {
	return &r53.Change{Action: aws.String(action), ResourceRecordSet: &r53.ResourceRecordSet{
		Name: aws.String(name), Type: aws.String(rrType), TTL: aws.Int64(60),
		ResourceRecords: []*r53.ResourceRecord{{Value: aws.String(value)}},
	}}
}

func waitInSync(t *testing.T, c *r53.Route53, id *string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := c.GetChange(&r53.GetChangeInput{Id: id})
		if err != nil {
			return err
		}
		if s := aws.StringValue(out.ChangeInfo.Status); s != r53.ChangeStatusInsync {
			return fmt.Errorf("change %s is %s", aws.StringValue(id), s)
		}
		return nil
	}, syncTimeout, 2*time.Second)
}

// activation reads, or with verify checks now, the zone's activation through
// the Spinifex extension, called as a custom SDK operation.
func activation(t *testing.T, c *r53.Route53, zoneID string, verify bool) (a struct{ State, Reason string }) {
	t.Helper()
	got, err := callActivation(c, zoneID, verify)
	require.NoError(t, err)
	require.NotNil(t, got)
	a.State, a.Reason = aws.StringValue(got.State), aws.StringValue(got.Reason)
	return a
}

func callActivation(c *r53.Route53, zoneID string, verify bool) (*ext.Activation, error) {
	id := aws.String(strings.TrimPrefix(zoneID, "/hostedzone/"))
	path := "/2013-04-01/hostedzone/{Id}/activation"
	if verify {
		out := &ext.VerifyHostedZoneDelegationOutput{}
		op := &request.Operation{Name: ext.ActionVerifyHostedZoneDelegation, HTTPMethod: http.MethodPost, HTTPPath: path}
		err := c.NewRequest(op, &ext.VerifyHostedZoneDelegationInput{Id: id}, out).Send()
		return out.Activation, err
	}
	out := &ext.GetHostedZoneActivationOutput{}
	op := &request.Operation{Name: ext.ActionGetHostedZoneActivation, HTTPMethod: http.MethodGet, HTTPPath: path}
	err := c.NewRequest(op, &ext.GetHostedZoneActivationInput{Id: id}, out).Send()
	return out.Activation, err
}

// deleteZone removes every record set but the apex NS and SOA, then the zone.
// A zone already gone is not an error, so it serves as a cleanup too. It
// returns the deletion's change, or nil when nothing was deleted.
func deleteZone(t *testing.T, c *r53.Route53, zoneID string) (changeID *string) {
	t.Helper()
	zone, err := c.GetHostedZone(&r53.GetHostedZoneInput{Id: aws.String(zoneID)})
	if err != nil {
		return nil
	}
	sets, err := c.ListResourceRecordSets(&r53.ListResourceRecordSetsInput{HostedZoneId: aws.String(zoneID)})
	if err != nil {
		t.Logf("list record sets of %s: %v", zoneID, err)
		return nil
	}
	apex := aws.StringValue(zone.HostedZone.Name)
	var changes []*r53.Change
	for _, s := range sets.ResourceRecordSets {
		if aws.StringValue(s.Name) == apex && slices.Contains([]string{"NS", "SOA"}, aws.StringValue(s.Type)) {
			continue
		}
		changes = append(changes, &r53.Change{Action: aws.String("DELETE"), ResourceRecordSet: s})
	}
	if len(changes) > 0 {
		if _, err := c.ChangeResourceRecordSets(&r53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(zoneID), ChangeBatch: &r53.ChangeBatch{Changes: changes},
		}); err != nil {
			t.Logf("delete record sets of %s: %v", zoneID, err)
		}
	}
	out, err := c.DeleteHostedZone(&r53.DeleteHostedZoneInput{Id: aws.String(zoneID)})
	if err != nil {
		t.Logf("delete hosted zone %s: %v", zoneID, err)
		return nil
	}
	return out.ChangeInfo.Id
}

func wantTXT(node harness.Node, name, want string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	txt, err := harness.NodeResolver(node).LookupTXT(ctx, name)
	if err != nil {
		return fmt.Errorf("TXT %s via %s: %w", name, node.Name, err)
	}
	if !slices.Contains(txt, want) {
		return fmt.Errorf("TXT %s via %s = %v, want %q", name, node.Name, txt, want)
	}
	return nil
}

// wantNS checks the base zone's apex NS set is exactly the delegation set a
// hosted zone is told to use, so a registrar given that set reaches the cluster.
func wantNS(node harness.Node, name string, want []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ns, err := harness.NodeResolver(node).LookupNS(ctx, name)
	if err != nil {
		return fmt.Errorf("NS %s via %s: %w", name, node.Name, err)
	}
	var got, exp []string
	for _, n := range ns {
		got = append(got, strings.ToLower(strings.TrimSuffix(n.Host, ".")))
	}
	for _, n := range want {
		exp = append(exp, strings.ToLower(strings.TrimSuffix(n, ".")))
	}
	slices.Sort(got)
	slices.Sort(exp)
	if !slices.Equal(got, exp) {
		return fmt.Errorf("NS %s via %s = %v, want %v", name, node.Name, got, exp)
	}
	return nil
}

func wantMX(node harness.Node, name, host string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mx, err := harness.NodeResolver(node).LookupMX(ctx, name)
	if err != nil {
		return fmt.Errorf("MX %s via %s: %w", name, node.Name, err)
	}
	for _, m := range mx {
		if strings.EqualFold(m.Host, host) && m.Pref == mxPref {
			return nil
		}
	}
	return fmt.Errorf("MX %s via %s has no %d %s", name, node.Name, mxPref, host)
}

// newTenant creates an account the way self-service signup does, with a
// client scoped to it.
func newTenant(t *testing.T, env *harness.Env, label string) *harness.Profile {
	t.Helper()
	info := harness.SpxAdminAccountCreate(t, fmt.Sprintf("E2E %s %d", label, time.Now().UnixNano()), "")
	return harness.NewAccountCarousel().Add(t, env, label, info)
}
