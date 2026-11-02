package codestore

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAssume struct {
	calls   int
	err     error
	expires time.Time
}

func (f *fakeAssume) assume() (*sts.AssumeRoleOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &sts.AssumeRoleOutput{Credentials: &sts.Credentials{
		AccessKeyId:     aws.String("ASIA" + string(rune('A'+f.calls))),
		SecretAccessKey: aws.String("secret"),
		SessionToken:    aws.String("token"),
		Expiration:      aws.Time(f.expires),
	}}, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestSessionProvider_ReusesUntilRefreshMargin(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a := &fakeAssume{expires: c.t.Add(time.Hour)}
	p := newSessionProvider(a.assume, c.now)

	first, err := p.Retrieve()
	require.NoError(t, err)
	c.t = c.t.Add(54 * time.Minute)
	assert.False(t, p.IsExpired())
	again, err := p.Retrieve()
	require.NoError(t, err)
	assert.Equal(t, first.AccessKeyID, again.AccessKeyID)
	assert.Equal(t, 1, a.calls)

	c.t = c.t.Add(2 * time.Minute)
	a.expires = c.t.Add(time.Hour)
	assert.True(t, p.IsExpired())
	fresh, err := p.Retrieve()
	require.NoError(t, err)
	assert.NotEqual(t, first.AccessKeyID, fresh.AccessKeyID)
	assert.Equal(t, 2, a.calls)
}

// A failed re-assume keeps the still-valid session, spaces retries, and fails
// closed once the session has expired.
func TestSessionProvider_KeepsSessionOnFailureUntilExpiry(t *testing.T) {
	c := &clock{t: time.Unix(1_000_000, 0)}
	a := &fakeAssume{expires: c.t.Add(time.Hour)}
	p := newSessionProvider(a.assume, c.now)
	first, err := p.Retrieve()
	require.NoError(t, err)

	a.err = errors.New("sts unavailable")
	c.t = c.t.Add(58 * time.Minute)
	kept, err := p.Retrieve()
	require.NoError(t, err)
	assert.Equal(t, first.AccessKeyID, kept.AccessKeyID)
	assert.Equal(t, 2, a.calls)

	c.t = c.t.Add(10 * time.Second)
	_, err = p.Retrieve()
	require.NoError(t, err)
	assert.Equal(t, 2, a.calls, "no re-assume inside the retry interval")

	c.t = c.t.Add(2 * time.Minute)
	_, err = p.Retrieve()
	require.Error(t, err)
	assert.Equal(t, 3, a.calls)
}

func TestSessionProvider_FirstFailureFails(t *testing.T) {
	a := &fakeAssume{err: errors.New("AccessDenied")}
	p := newSessionProvider(a.assume, time.Now)

	_, err := p.Retrieve()
	require.Error(t, err)
	assert.True(t, p.IsExpired())
}
