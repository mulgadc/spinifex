package workerboot

import (
	"context"

	"github.com/mulgadc/spinifex/internal/sqsworkergw"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
)

// GatewayClaimer adapts the signed gateway client to the receipt key decision.
type GatewayClaimer struct{ Client *sqsworkergw.Client }

var _ workerkey.Claimer = GatewayClaimer{}

func (c GatewayClaimer) Claim(ctx context.Context) (workerkey.Decision, error) {
	k, err := c.Client.ClaimReceiptKey(ctx)
	return workerkey.Decision{Pending: k.Pending, Fingerprint: k.Fingerprint, HostNodes: k.HostNodes}, err
}

func (c GatewayClaimer) RecordFingerprint(ctx context.Context, fingerprint string) error {
	return c.Client.RecordReceiptKey(ctx, fingerprint)
}
