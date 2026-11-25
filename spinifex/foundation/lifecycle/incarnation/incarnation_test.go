package incarnation

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
)

const (
	current = "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/ng1/new"
	earlier = "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/ng1/old"
)

func TestPermits(t *testing.T) {
	unbound := context.Background()
	cases := []struct {
		name   string
		ctx    context.Context
		stored string
		want   bool
	}{
		{"unbound request acts on any record", unbound, current, true},
		{"unbound request acts on no record", unbound, "", true},
		{"bound to the stored record", With(unbound, current), current, true},
		{"bound to an earlier incarnation", With(unbound, earlier), current, false},
		{"bound to an absent record that has since appeared", With(unbound, ""), current, false},
		{"bound to an absent record that is still absent", With(unbound, ""), "", true},
		{"bound to a record that has since gone", With(unbound, current), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Permits(tc.ctx, tc.stored))
		})
	}
}

// The binding has to survive the NATS hop, including the absent case.
func TestFromMsgRestoresTheBinding(t *testing.T) {
	for _, bound := range []string{current, ""} {
		sent, _ := FromContext(With(context.Background(), bound))
		msg := nats.NewMsg("eks.DescribeNodegroup")
		msg.Header.Set(Header, sent)

		ctx := FromMsg(context.Background(), msg)
		assert.True(t, Permits(ctx, bound))
		assert.False(t, Permits(ctx, earlier))
	}
	_, ok := FromContext(FromMsg(context.Background(), nats.NewMsg("eks.DescribeNodegroup")))
	assert.False(t, ok, "a message with no header must carry no binding")
}
