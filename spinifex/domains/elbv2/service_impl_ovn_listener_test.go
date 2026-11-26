package elbv2

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tcpGroup creates a TCP instance target group in the fixture VPC through the
// service and clears the recorded calls.
func (h *ovnCreateHarness) tcpGroup(name string, port int64) *elbv2.TargetGroup {
	out, err := h.svc.CreateTargetGroup(h.t.Context(), &elbv2.CreateTargetGroupInput{
		Name: aws.String(name), Protocol: aws.String(ProtocolTCP), Port: aws.Int64(port),
		VpcId: aws.String(h.vpcID), TargetType: aws.String(TargetTypeInstance),
	}, testAccountID)
	require.NoError(h.t, err)
	require.Len(h.t, out.TargetGroups, 1)
	h.takeOps()
	return out.TargetGroups[0]
}

// storedGroup writes a target group record directly, for shapes the service
// cannot create or change on the fixture path.
func (h *ovnCreateHarness) storedGroup(id string, edit func(*TargetGroupRecord)) string {
	tg := &TargetGroupRecord{
		TargetGroupArn: resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, "tg-"+id[:4], id),
		TargetGroupID:  id, Name: "tg-" + id[:4], Protocol: ProtocolTCP, Port: 8080, VpcId: h.vpcID,
		TargetType: TargetTypeInstance, AccountID: testAccountID, CreatedAt: time.Now(),
	}
	edit(tg)
	require.NoError(h.t, h.svc.store.PutTargetGroup(h.t.Context(), tg))
	h.takeOps()
	return tg.TargetGroupArn
}

func groupID(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

func listenerInput(lbArn, tgArn string, port int64) *elbv2.CreateListenerInput {
	return &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn), Protocol: aws.String(ProtocolTCP), Port: aws.Int64(port),
		DefaultActions: []*elbv2.Action{{Type: aws.String(ActionTypeForward), TargetGroupArn: aws.String(tgArn)}},
	}
}

func (h *ovnCreateHarness) listener(input *elbv2.CreateListenerInput) (*elbv2.Listener, error) {
	out, err := h.svc.CreateListener(h.t.Context(), input, testAccountID)
	if err != nil {
		return nil, err
	}
	require.Len(h.t, out.Listeners, 1)
	return out.Listeners[0], nil
}

func (h *ovnCreateHarness) owner(name string) *fixtureOwner {
	owner, err := h.svc.ovn.owners.GetSealedOwner(h.t.Context(), name, testAccountID)
	require.NoError(h.t, err)
	require.NotNil(h.t, owner)
	return owner
}

// requireNoEffects asserts the calls since the last take wrote and listed
// nothing, and that no bucket, ENI, group, route, allocation or launch changed.
func (h *ovnCreateHarness) requireNoEffects(elbBefore map[string]uint64, sideBefore map[string]map[string]uint64) {
	h.t.Helper()
	for bucket, calls := range h.takeOps() {
		assert.Empty(h.t, opsOf(calls, opKVWrites), "write on %s", bucket)
		assert.Empty(h.t, opsOf(calls, opKVLists), "enumeration on %s", bucket)
	}
	assert.Equal(h.t, elbBefore, h.state(KVBucketELBv2))
	assert.Equal(h.t, sideBefore, h.sideEffects())
	assert.Zero(h.t, h.launches())
}

func TestOVNFixtureListener_CreateSealsOneListenerAndRepeats(t *testing.T) {
	h := newOVNCreateHarness(t)
	created := h.sealedOVN("ovn-nlb")
	lbArn := aws.StringValue(created.LoadBalancerArn)
	tg := h.tcpGroup("ovn-tg", 8080)
	tgArn := aws.StringValue(tg.TargetGroupArn)
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	nameKey := LBNameKey("ovn-nlb", testAccountID)
	tgKey := KeyPrefixTG + groupID(tgArn)

	got, err := h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(t, err)
	listenerArn := aws.StringValue(got.ListenerArn)
	parsed, ok := resourcearn.ParseELBv2(listenerArn)
	require.True(t, ok)
	assert.Equal(t, resourcearn.ELBv2Listener, parsed.Kind)
	assert.Equal(t, &elbv2.Listener{
		ListenerArn: aws.String(listenerArn), LoadBalancerArn: aws.String(lbArn),
		Protocol: aws.String(ProtocolTCP), Port: aws.Int64(80),
		DefaultActions: []*elbv2.Action{{Type: aws.String(ActionTypeForward), TargetGroupArn: aws.String(tgArn)}},
	}, got)
	h.requireCalls(map[string][]string{KVBucketELBv2: {"Get " + nameKey, "Get " + tgKey, "Get " + nameKey, "Update " + nameKey}})

	owner := h.owner("ovn-nlb")
	assert.Equal(t, uint64(2), owner.OwnerRevision)
	assert.Equal(t, &fixtureListener{ListenerArn: listenerArn, Port: 80, TargetGroupArn: tgArn, TargetPort: 8080}, owner.Listener)
	after := h.state(KVBucketELBv2)
	assert.Greater(t, after[nameKey], elbBefore[nameKey])
	after[nameKey] = elbBefore[nameKey]
	assert.Equal(t, elbBefore, after, "only the owner row changes: no listener or group key")
	assert.Equal(t, sideBefore, h.sideEffects(), "no ENI, group, route or allocation write")
	assert.Zero(t, h.launches())

	desc, err := h.describe(testAccountID, byARN(lbArn))
	require.NoError(t, err)
	assert.Equal(t, created, desc, "the load balancer stays provisioning")

	elbBefore = h.state(KVBucketELBv2)
	h.takeOps()
	again, err := h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(t, err, "the same settings repeat")
	assert.Equal(t, got, again)
	h.requireCalls(map[string][]string{KVBucketELBv2: {"Get " + nameKey, "Get " + tgKey, "Get " + nameKey}})
	assert.Equal(t, elbBefore, h.state(KVBucketELBv2))
	assert.Equal(t, uint64(2), h.owner("ovn-nlb").OwnerRevision)

	h.restart()
	again, err = h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(t, err)
	assert.Equal(t, got, again, "a restarted service reads the same listener")
}

func TestOVNFixtureListener_RefusesRawShapesBeforeReads(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	with := func(edit func(*elbv2.CreateListenerInput)) *elbv2.CreateListenerInput {
		in := listenerInput(lbArn, tgArn, 80)
		edit(in)
		return in
	}
	action := func(edit func(*elbv2.Action)) *elbv2.CreateListenerInput {
		return with(func(in *elbv2.CreateListenerInput) { edit(in.DefaultActions[0]) })
	}
	invalid := awserrors.ErrorELBv2InvalidConfigurationRequest
	forward := &elbv2.ForwardActionConfig{TargetGroups: []*elbv2.TargetGroupTuple{{TargetGroupArn: aws.String(tgArn)}}}

	for name, tc := range map[string]struct {
		input *elbv2.CreateListenerInput
		code  string
	}{
		"no load balancer":  {with(func(in *elbv2.CreateListenerInput) { in.LoadBalancerArn = nil }), awserrors.ErrorMissingParameter},
		"no default action": {with(func(in *elbv2.CreateListenerInput) { in.DefaultActions = nil }), awserrors.ErrorMissingParameter},
		"no protocol":       {with(func(in *elbv2.CreateListenerInput) { in.Protocol = nil }), invalid},
		"UDP":               {with(func(in *elbv2.CreateListenerInput) { in.Protocol = aws.String(ProtocolUDP) }), invalid},
		"TLS":               {with(func(in *elbv2.CreateListenerInput) { in.Protocol = aws.String(ProtocolTLS) }), invalid},
		"no port":           {with(func(in *elbv2.CreateListenerInput) { in.Port = nil }), awserrors.ErrorValidationError},
		"port 0":            {with(func(in *elbv2.CreateListenerInput) { in.Port = aws.Int64(0) }), awserrors.ErrorValidationError},
		"port 65536":        {with(func(in *elbv2.CreateListenerInput) { in.Port = aws.Int64(65536) }), awserrors.ErrorValidationError},
		"certificate": {with(func(in *elbv2.CreateListenerInput) {
			in.Certificates = []*elbv2.Certificate{{CertificateArn: aws.String("c")}}
		}), invalid},
		"SSL policy":            {with(func(in *elbv2.CreateListenerInput) { in.SslPolicy = aws.String(DefaultSslPolicy) }), invalid},
		"ALPN policy":           {with(func(in *elbv2.CreateListenerInput) { in.AlpnPolicy = aws.StringSlice([]string{"None"}) }), invalid},
		"mutual authentication": {with(func(in *elbv2.CreateListenerInput) { in.MutualAuthentication = &elbv2.MutualAuthenticationAttributes{} }), invalid},
		"tags":                  {with(func(in *elbv2.CreateListenerInput) { in.Tags = []*elbv2.Tag{{Key: aws.String("k")}} }), invalid},
		"two actions": {with(func(in *elbv2.CreateListenerInput) {
			in.DefaultActions = append(in.DefaultActions, in.DefaultActions[0])
		}), invalid},
		"nil action":            {with(func(in *elbv2.CreateListenerInput) { in.DefaultActions[0] = nil }), awserrors.ErrorValidationError},
		"no action type":        {action(func(a *elbv2.Action) { a.Type = nil }), invalid},
		"fixed response":        {action(func(a *elbv2.Action) { a.Type = aws.String(ActionTypeFixedResponse) }), invalid},
		"forward config":        {action(func(a *elbv2.Action) { a.ForwardConfig = forward }), invalid},
		"forward config only":   {action(func(a *elbv2.Action) { a.TargetGroupArn, a.ForwardConfig = nil, forward }), invalid},
		"empty forward config":  {action(func(a *elbv2.Action) { a.ForwardConfig = &elbv2.ForwardActionConfig{} }), invalid},
		"fixed response config": {action(func(a *elbv2.Action) { a.FixedResponseConfig = &elbv2.FixedResponseActionConfig{} }), invalid},
		"redirect config":       {action(func(a *elbv2.Action) { a.RedirectConfig = &elbv2.RedirectActionConfig{} }), invalid},
		"cognito config":        {action(func(a *elbv2.Action) { a.AuthenticateCognitoConfig = &elbv2.AuthenticateCognitoActionConfig{} }), invalid},
		"OIDC config":           {action(func(a *elbv2.Action) { a.AuthenticateOidcConfig = &elbv2.AuthenticateOidcActionConfig{} }), invalid},
		"order":                 {action(func(a *elbv2.Action) { a.Order = aws.Int64(1) }), invalid},
		"no target group":       {action(func(a *elbv2.Action) { a.TargetGroupArn = nil }), awserrors.ErrorValidationError},
		"empty target group":    {action(func(a *elbv2.Action) { a.TargetGroupArn = aws.String("") }), awserrors.ErrorValidationError},
		"other account group ARN": {action(func(a *elbv2.Action) {
			a.TargetGroupArn = aws.String(resourcearn.FormatELBv2TargetGroup(fixtureRegion, otherAccountID, "x", "eeee000000000005"))
		}), ""},
		"nil input": {nil, awserrors.ErrorMissingParameter},
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			_, err := h.svc.CreateListener(t.Context(), tc.input, testAccountID)
			if tc.code == "" {
				requireFenceCode(t, err, awserrors.ErrorELBv2TargetGroupNotFound)
				h.requireCalls(map[string][]string{KVBucketELBv2: {"Get " + LBNameKey("ovn-nlb", testAccountID)}})
			} else {
				requireFenceCode(t, err, tc.code)
				h.requireCalls(map[string][]string{})
			}
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}
	assert.Nil(t, h.owner("ovn-nlb").Listener)
}

func TestOVNFixtureListener_MalformedGroupARNReadsNoGroup(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	group := func(resource string) string {
		return resourcearn.FormatELBv2Resource(fixtureRegion, testAccountID, resourcearn.ELBv2TargetGroup, resource)
	}

	for name, arn := range map[string]string{
		"wildcard ID":     group("ovn-tg/tg-*"),
		"subject ID":      group("ovn-tg/tg->"),
		"space in ID":     group("ovn-tg/tg 01"),
		"dotted ID":       group("ovn-tg/tg.01"),
		"empty ID":        group("ovn-tg/"),
		"no name":         group("/tg-0123456789abcdef0"),
		"no ID segment":   group("tg-0123456789abcdef0"),
		"empty resource":  group(""),
		"unicode ID":      group("ovn-tg/tg-é"),
		"ID with newline": group("ovn-tg/tg-01\n"),
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			_, err := h.listener(listenerInput(lbArn, arn, 80))
			requireFenceCode(t, err, awserrors.ErrorELBv2TargetGroupNotFound)
			h.requireCalls(map[string][]string{KVBucketELBv2: {"Get " + LBNameKey("ovn-nlb", testAccountID)}})
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}

}

func TestOVNFixtureListener_PersistedGroupARNFormsStayAccepted(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	for name, arn := range map[string]string{
		"service ID":        aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn),
		"bare hex ID":       h.storedGroup("dddd000000000004", func(*TargetGroupRecord) {}),
		"unvalidated name":  h.storedGroup("eeee000000000005", func(tg *TargetGroupRecord) { tg.Name = "my.group" }),
		"name with a slash": resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, "a/b", "tg-0123456789abcdef0"),
		"mixed case hex ID": resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, "ovn-tg", "ABCDEF0123456789"),
	} {
		assert.NoError(t, ovnGroupARN(arn, fixtureRegion, testAccountID), name)
	}

	bare := resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, "tg-dddd", "dddd000000000004")
	got, err := h.listener(listenerInput(lbArn, bare, 80))
	require.NoError(t, err, "a stored bare hex group is admitted")
	assert.Equal(t, bare, aws.StringValue(got.DefaultActions[0].TargetGroupArn))
}

func TestOVNFixtureListener_RefusesLoadBalancerAndGroupWithoutWriting(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	groups := map[string]string{
		"udp":     h.storedGroup("a000000000000001", func(tg *TargetGroupRecord) { tg.Protocol = ProtocolUDP }),
		"ip":      h.storedGroup("a000000000000002", func(tg *TargetGroupRecord) { tg.TargetType = TargetTypeIP }),
		"vpc":     h.storedGroup("a000000000000003", func(tg *TargetGroupRecord) { tg.VpcId = "vpc-other" }),
		"port":    h.storedGroup("a000000000000004", func(tg *TargetGroupRecord) { tg.Port = 0 }),
		"targets": h.storedGroup("a000000000000005", func(tg *TargetGroupRecord) { tg.Targets = []Target{{Id: "i-0123", Port: 8080}} }),
		"account": h.storedGroup("a000000000000006", func(tg *TargetGroupRecord) { tg.AccountID = otherAccountID }),
	}
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	invalid := awserrors.ErrorELBv2InvalidConfigurationRequest

	for name, tc := range map[string]struct {
		account, lb, tg, code string
	}{
		"other account caller": {otherAccountID, lbArn, tgArn, awserrors.ErrorELBv2LoadBalancerNotFound},
		"unknown load balancer": {testAccountID, resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, "ovn-other", "cccc000000000003", LoadBalancerTypeNetwork),
			tgArn, awserrors.ErrorELBv2LoadBalancerNotFound},
		"missing group":       {testAccountID, lbArn, resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, "gone", "dddd000000000004"), awserrors.ErrorELBv2TargetGroupNotFound},
		"other region group":  {testAccountID, lbArn, resourcearn.FormatELBv2TargetGroup("us-east-1", testAccountID, "ovn-tg", groupID(tgArn)), awserrors.ErrorELBv2TargetGroupNotFound},
		"other account group": {testAccountID, lbArn, groups["account"], awserrors.ErrorELBv2TargetGroupNotFound},
		"UDP group":           {testAccountID, lbArn, groups["udp"], awserrors.ErrorELBv2IncompatibleProtocols},
		"IP group":            {testAccountID, lbArn, groups["ip"], invalid},
		"other VPC group":     {testAccountID, lbArn, groups["vpc"], invalid},
		"invalid group port":  {testAccountID, lbArn, groups["port"], invalid},
		"registered targets":  {testAccountID, lbArn, groups["targets"], invalid},
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			_, err := h.svc.CreateListener(t.Context(), listenerInput(tc.lb, tc.tg, 80), tc.account)
			requireFenceCode(t, err, tc.code)
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}

	t.Run("failed group read", func(t *testing.T) {
		h.takeOps()
		h.elbKV.setFailGetPrefix(KeyPrefixTG, errInjectedRead)
		defer h.elbKV.setFailGet(nil)
		_, err := h.listener(listenerInput(lbArn, tgArn, 80))
		requireFenceCode(t, err, awserrors.ErrorServerInternal)
		h.requireNoEffects(elbBefore, sideBefore)
	})
	t.Run("failed root read", func(t *testing.T) {
		h.takeOps()
		h.elbKV.setFailGetPrefix(KeyPrefixLBName, errInjectedRead)
		defer h.elbKV.setFailGet(nil)
		_, err := h.listener(listenerInput(lbArn, tgArn, 80))
		requireFenceCode(t, err, awserrors.ErrorServerInternal)
		h.requireNoEffects(elbBefore, sideBefore)
	})
	assert.Nil(t, h.owner("ovn-nlb").Listener)
}

func TestOVNFixtureListener_OneListenerPerLoadBalancer(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	otherArn := aws.StringValue(h.tcpGroup("ovn-tg2", 9090).TargetGroupArn)
	first, err := h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(t, err)
	want := h.owner("ovn-nlb")
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()

	for name, tc := range map[string]struct {
		input *elbv2.CreateListenerInput
		code  string
	}{
		"same port, other group": {listenerInput(lbArn, otherArn, 80), awserrors.ErrorELBv2DuplicateListener},
		"second port":            {listenerInput(lbArn, tgArn, 81), awserrors.ErrorELBv2TooManyListeners},
		"second port and group":  {listenerInput(lbArn, otherArn, 81), awserrors.ErrorELBv2TooManyListeners},
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			_, err := h.listener(tc.input)
			requireFenceCode(t, err, tc.code)
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}
	assert.Equal(t, want, h.owner("ovn-nlb"))
	again, err := h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(t, err)
	assert.Equal(t, first, again)
}

func TestOVNFixtureListener_CompetingCreates(t *testing.T) {
	for name, tc := range map[string]struct {
		port  int64
		other bool
		code  string
	}{
		"same settings":    {80, false, ""},
		"same port, other": {80, true, awserrors.ErrorELBv2DuplicateListener},
		"second port":      {81, false, awserrors.ErrorELBv2TooManyListeners},
	} {
		t.Run(name, func(t *testing.T) {
			h := newOVNCreateHarness(t)
			lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
			tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
			competitorArn := tgArn
			if tc.other {
				competitorArn = aws.StringValue(h.tcpGroup("ovn-tg2", 9090).TargetGroupArn)
			}
			var winner *elbv2.Listener
			h.elbKV.interceptUpdate(KeyPrefixLBName, func(_ string, _ []byte, update func() (uint64, error)) (uint64, error) {
				var err error
				winner, err = h.listener(listenerInput(lbArn, competitorArn, tc.port))
				require.NoError(t, err)
				return update()
			})

			got, err := h.listener(listenerInput(lbArn, tgArn, 80))
			require.NotNil(t, winner)
			owner := h.owner("ovn-nlb")
			assert.Equal(t, uint64(2), owner.OwnerRevision, "only the winner's edit is durable")
			assert.Equal(t, aws.StringValue(winner.ListenerArn), owner.Listener.ListenerArn)
			assert.Equal(t, tc.port, owner.Listener.Port)
			if tc.code != "" {
				requireFenceCode(t, err, tc.code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, winner, got, "the loser returns the winner's listener")
		})
	}
}

func TestOVNFixtureListener_UncertainEditIsServerError(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	elbBefore := h.state(KVBucketELBv2)
	h.elbKV.interceptUpdate(KeyPrefixLBName, func(string, []byte, func() (uint64, error)) (uint64, error) {
		return 0, errors.New("injected timeout")
	})

	_, err := h.listener(listenerInput(lbArn, tgArn, 80))
	requireFenceCode(t, err, awserrors.ErrorServerInternal)
	assert.Equal(t, elbBefore, h.state(KVBucketELBv2), "the unwritten edit leaves the owner as it was")
	assert.Nil(t, h.owner("ovn-nlb").Listener)

	_, err = h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(t, err, "a caller retry succeeds")
}

func TestOVNFixtureListener_DeleteRefusesHeldListener(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn)
	got, err := h.listener(listenerInput(lbArn, tgArn, 80))
	require.NoError(t, err)
	listenerArn := aws.StringValue(got.ListenerArn)
	want := h.owner("ovn-nlb")
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	h.takeOps()

	_, err = h.svc.DeleteListener(t.Context(), &elbv2.DeleteListenerInput{ListenerArn: aws.String(listenerArn)}, testAccountID)
	requireFenceCode(t, err, awserrors.ErrorOperationNotPermitted)
	h.requireCalls(map[string][]string{KVBucketELBv2: {"Get " + LBNameKey("ovn-nlb", testAccountID)}})
	h.requireNoEffects(elbBefore, sideBefore)
	assert.Equal(t, want, h.owner("ovn-nlb"))

	lb := want.Root
	for name, tc := range map[string]struct{ account, arn string }{
		"other account caller": {otherAccountID, listenerArn},
		"other listener ID":    {testAccountID, resourcearn.FormatELBv2Listener(fixtureRegion, testAccountID, lb.Name, lb.LoadBalancerID, "ffff000000000006", lb.Type)},
		"other load balancer":  {testAccountID, resourcearn.FormatELBv2Listener(fixtureRegion, testAccountID, "ovn-other", "cccc000000000003", "ffff000000000006", lb.Type)},
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			_, err := h.svc.DeleteListener(t.Context(), &elbv2.DeleteListenerInput{ListenerArn: aws.String(tc.arn)}, tc.account)
			require.NoError(t, err, "an absent or unowned listener keeps the idempotent result")
			h.requireNoEffects(elbBefore, sideBefore)
		})
	}
	t.Run("failed owner read", func(t *testing.T) {
		h.takeOps()
		h.elbKV.setFailGetPrefix(KeyPrefixLBName, errInjectedRead)
		defer h.elbKV.setFailGet(nil)
		_, err := h.svc.DeleteListener(t.Context(), &elbv2.DeleteListenerInput{ListenerArn: aws.String(listenerArn)}, testAccountID)
		requireFenceCode(t, err, awserrors.ErrorServerInternal)
		h.requireNoEffects(elbBefore, sideBefore)
	})
	assert.Equal(t, want, h.owner("ovn-nlb"))
}

func TestOVNFixtureListener_GroupChangesRefusedWithoutReads(t *testing.T) {
	h := newOVNCreateHarness(t)
	lbArn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	tgArn := aws.String(aws.StringValue(h.tcpGroup("ovn-tg", 8080).TargetGroupArn))
	ctx := t.Context()
	calls := map[string]func() error{
		"ModifyTargetGroup": func() error {
			_, err := h.svc.ModifyTargetGroup(ctx, &elbv2.ModifyTargetGroupInput{TargetGroupArn: tgArn, HealthCheckPort: aws.String("9000")}, testAccountID)
			return err
		},
		"DeleteTargetGroup": func() error {
			_, err := h.svc.DeleteTargetGroup(ctx, &elbv2.DeleteTargetGroupInput{TargetGroupArn: tgArn}, testAccountID)
			return err
		},
		"ModifyTargetGroupAttributes": func() error {
			_, err := h.svc.ModifyTargetGroupAttributes(ctx, &elbv2.ModifyTargetGroupAttributesInput{
				TargetGroupArn: tgArn, Attributes: []*elbv2.TargetGroupAttribute{{Key: aws.String("deregistration_delay.timeout_seconds"), Value: aws.String("10")}},
			}, testAccountID)
			return err
		},
	}
	check := func(t *testing.T) {
		elbBefore := h.state(KVBucketELBv2)
		sideBefore := h.sideEffects()
		for name, call := range calls {
			t.Run(name, func(t *testing.T) {
				h.takeOps()
				requireFenceCode(t, call(), awserrors.ErrorOperationNotPermitted)
				h.requireCalls(map[string][]string{})
				h.requireNoEffects(elbBefore, sideBefore)
			})
		}
	}

	t.Run("before listener", check)
	_, err := h.listener(listenerInput(lbArn, *tgArn, 80))
	require.NoError(t, err)
	t.Run("while listener held", check)
	tg, err := h.svc.store.GetTargetGroupByArn(ctx, *tgArn)
	require.NoError(t, err)
	require.NotNil(t, tg, "the group survives")
	assert.Empty(t, tg.Targets)
}
