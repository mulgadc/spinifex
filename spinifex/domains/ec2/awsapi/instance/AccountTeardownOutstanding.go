package instance

import (
	"context"
	"fmt"
	"time"

	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// AccountTeardownOutstandingSubject serves the instances of an account whose
// teardown is not complete, from the shared record store.
const AccountTeardownOutstandingSubject = "ec2.AccountTeardownOutstanding"

const accountTeardownOutstandingTimeout = 15 * time.Second

// AccountTeardownOutstanding reads the account's unfinished instance teardowns.
// No responder, a timeout or an error reply is an error, never an empty list.
func AccountTeardownOutstanding(ctx context.Context, natsConn *nats.Conn, accountID string) ([]ec2instance.OutstandingInstance, error) {
	out, err := natsmsg.NATSRequest[ec2instance.TeardownOutstandingOutput](ctx, natsConn, AccountTeardownOutstandingSubject,
		&ec2instance.TeardownOutstandingInput{}, accountTeardownOutstandingTimeout, accountID)
	if err != nil {
		return nil, fmt.Errorf("read outstanding teardown: %w", err)
	}
	return out.Instances, nil
}
