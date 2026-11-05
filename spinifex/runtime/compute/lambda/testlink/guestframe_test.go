//go:build lambdatestlink

//test:in-package — drives Daemon.Handle with the package's grant fixtures and guestRequest
package testlink

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
)

// maxCredential encodes to exactly MaxCredentialBytes.
func maxCredential(t *testing.T) Credential {
	t.Helper()
	c := Credential{AccessKeyID: "ASIA" + strings.Repeat("K", 16), SecretAccessKey: strings.Repeat("s", 40)}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	c.SessionToken = strings.Repeat("t", MaxCredentialBytes-len(b))
	b, err = json.Marshal(c)
	require.NoError(t, err)
	require.Len(t, b, MaxCredentialBytes)
	return c
}

// A request the control socket accepts but whose guest frame would not fit
// is refused before Start, so no grant starts and nothing is allocated.
func TestDaemon_GuestFrameOverflowRefusedBeforeStart(t *testing.T) {
	code := []byte("zip bytes")
	good := codeServer(t, code)
	g := grant(t, code)
	ips, taps := &fakeIPs{}, &fakeTaps{}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: good.Client(), WorkDir: t.TempDir()})

	req := baseRequest(good.URL, code)
	req.Grant, req.ImageDigest = g.att.Grant, g.image.Digest
	req.Payload = make([]byte, MaxPayloadBytes)
	req.FunctionARN = strings.Repeat("a", 60000)
	b, err := json.Marshal(req)
	require.NoError(t, err)
	require.LessOrEqual(t, int64(len(b)), MaxFrameBytes, "the control frame fits")
	require.NoError(t, CheckRequest(req))

	rep := d.Handle(context.Background(), b, maxCredential(t))
	require.Equal(t, OutcomeRefused, rep.Outcome, rep.Error)
	require.Contains(t, rep.Error, "does not fit the guest link")
	require.Equal(t, execution.StateGranted, g.state(t), "the grant was never started")
	require.Empty(t, ips.allocated)
	require.Empty(t, taps.created)
}

// The largest Invoke ARN, a full payload, a maximal credential and full IDs
// reach a guest reader byte-identical through HostSend.
func TestGuestRequest_LargestInvokeFitsTheGuestLink(t *testing.T) {
	cred := maxCredential(t)
	req := Request{
		Grant:     execution.Grant{GrantID: strings.Repeat("f", 32)},
		AttemptID: strings.Repeat("a", 12),
		RequestID: strings.Repeat("b", 12),
		// Region of representative length; name and qualifier at Invoke's maxima.
		FunctionARN: "arn:aws:lambda:ap-southeast-2:123456789012:function:" + strings.Repeat("n", 64) + ":" + strings.Repeat("q", 128),
		Payload:     make([]byte, MaxPayloadBytes),
		cred:        cred,
	}
	for i := range req.Payload {
		req.Payload[i] = byte(i)
	}
	sent := guestRequest(req, math.MaxInt64)
	require.NoError(t, hostlink.CheckInvokeFits(sent))

	hostEnd, guestEnd := net.Pipe()
	t.Cleanup(func() { _ = hostEnd.Close(); _ = guestEnd.Close() })
	host, guest := hostlink.NewTestStream(hostEnd), hostlink.NewTestStream(guestEnd)
	sendErr := make(chan error, 1)
	go func() { sendErr <- host.HostSend(sent) }()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	got, err := guest.Next(ctx)
	require.NoError(t, err)
	require.NoError(t, <-sendErr)
	require.Equal(t, sent, got)
}
