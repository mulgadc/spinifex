package handlers_imds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each absent-role case must be distinguishable. Before this the three collapsed
// into one nil and the credential path failed with nothing in the logs.
func TestProfileForReportsWhyTheRoleIsMissing(t *testing.T) {
	tests := []struct {
		name string
		res  *fakeResolver
		iam  *fakeIAM
		want roleMiss
	}{
		{
			name: "instance not visible",
			res:  &fakeResolver{eni: testENI()},
			iam:  &fakeIAM{},
			want: roleMissInstanceUnresolved,
		},
		{
			name: "no profile attached",
			res:  &fakeResolver{eni: testENI(), inst: &instanceFacts{}},
			iam:  &fakeIAM{},
			want: roleMissNoProfile,
		},
		{
			name: "profile deleted from IAM",
			res:  &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/gone"}},
			iam:  &fakeIAM{profileErr: errors.New(awserrors.ErrorIAMNoSuchEntity)},
			want: roleMissProfileDeleted,
		},
		{
			name: "role resolved",
			res:  &fakeResolver{eni: testENI(), inst: &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app"}},
			iam:  &fakeIAM{profile: &handlers_iam.InstanceProfile{ARN: "arn", InstanceProfileID: "AIPA", RoleName: "app"}},
			want: roleMissNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(tc.res, tc.iam, &fakeAssumer{})

			profile, miss, err := svc.profileFor(context.Background(), testENI())

			require.NoError(t, err)
			assert.Equal(t, tc.want, miss)
			assert.Equal(t, tc.want == roleMissNone, profile != nil)
		})
	}
}

// A backend failure is an error, not a miss: it 500s rather than answering an
// empty role list, so attributing it to the guest would be wrong.
func TestProfileForKeepsBackendFailuresAsErrors(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI(), instErr: errors.New("nats timeout")}, &fakeIAM{}, &fakeAssumer{})

	profile, miss, err := svc.profileFor(context.Background(), testENI())

	require.Error(t, err)
	assert.Nil(t, profile)
	assert.Equal(t, roleMissNone, miss)
}

// One stuck guest produced 42,517 role-list requests over five days, so the
// warning has to survive that volume without burying itself.
func TestRoleMissLoggerThrottlesPerInstanceAndReason(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	l := newRoleMissLogger(func() time.Time { return now })
	eni := testENI()

	assert.True(t, l.warn(context.Background(), eni, roleMissInstanceUnresolved), "first miss must log")
	assert.False(t, l.warn(context.Background(), eni, roleMissInstanceUnresolved), "repeat inside the interval must not")
	assert.True(t, l.warn(context.Background(), eni, roleMissNoProfile), "a different reason is a different fault")

	other := testENI()
	other.instanceID = "i-9999999999"
	assert.True(t, l.warn(context.Background(), other, roleMissInstanceUnresolved), "a second instance must log on its own")

	now = now.Add(roleMissLogInterval + time.Second)
	assert.True(t, l.warn(context.Background(), eni, roleMissInstanceUnresolved), "past the interval it must log again")
}

// roleMissNone is the resolved case and must never produce a line.
func TestRoleMissLoggerIgnoresResolvedRoles(t *testing.T) {
	l := newRoleMissLogger(time.Now)
	assert.False(t, l.warn(context.Background(), testENI(), roleMissNone))
}

// The map is keyed by instance, so without a sweep a long-lived vpcd would
// accumulate an entry for every instance that ever missed.
func TestRoleMissLoggerSweepBoundsTheMap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	l := newRoleMissLogger(func() time.Time { return now })

	require.True(t, l.warn(context.Background(), testENI(), roleMissInstanceUnresolved))
	l.sweep(now.Add(roleMissLogInterval))
	assert.Len(t, l.seen, 1, "an entry inside the window is still throttling")

	l.sweep(now.Add(3 * roleMissLogInterval))
	assert.Empty(t, l.seen)
}

// The empty role list is what an SDK reads as "no IMDS role" before it goes on
// signing with credentials it cannot refresh. That answer must not be silent.
func TestSecurityCredentialsListLogsAnUnresolvableRole(t *testing.T) {
	svc, _ := newTestService(&fakeResolver{eni: testENI()}, &fakeIAM{}, &fakeAssumer{})

	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	rec := httptest.NewRecorder()
	svc.serveSecurityCredentialsList(context.Background(), rec, testENI())

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())

	var line map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line))
	assert.Equal(t, string(roleMissInstanceUnresolved), line["reason"])
	assert.Equal(t, "i-0123456789", line["instance_id"])
	assert.Equal(t, "eni-aaa", line["eni_id"])
}
