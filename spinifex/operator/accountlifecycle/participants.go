package accountlifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	accountlifecyclev1 "github.com/mulgadc/spinifex/contracts/accountlifecycle/v1"
	migrationv1 "github.com/mulgadc/spinifex/contracts/migration/v1"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// participantRequestTimeout bounds one inventory or command request.
const participantRequestTimeout = 30 * time.Second

// participantDeclarations is the participant set, in order. Each declaration
// comes from the participant's own contract package. Every other domain is
// still handled by the legacy reapers and the lifecycle register.
func participantDeclarations() []accountlifecyclev1.Declaration {
	return []accountlifecyclev1.Declaration{migrationv1.Declaration()}
}

// Participant is one composed participant: its declaration and the client
// that reaches its inventory and command.
type Participant struct {
	Declaration accountlifecyclev1.Declaration
	Client      ParticipantClient
}

// ParticipantClient reaches one participant's inventory and command.
type ParticipantClient interface {
	Inventory(ctx context.Context, req accountlifecyclev1.InventoryRequest) (*accountlifecyclev1.InventoryReply, error)
	Terminate(ctx context.Context, req accountlifecyclev1.TerminateRequest) (*accountlifecyclev1.TerminateReply, error)
}

// natsParticipantClient sends each request to the participant's subjects.
// No responder, a timeout or an error reply is an error.
type natsParticipantClient struct {
	nc      *nats.Conn
	name    string
	timeout time.Duration
}

var _ ParticipantClient = (*natsParticipantClient)(nil)

func (c *natsParticipantClient) Inventory(ctx context.Context, req accountlifecyclev1.InventoryRequest) (*accountlifecyclev1.InventoryReply, error) {
	return natsmsg.NATSRequest[accountlifecyclev1.InventoryReply](ctx, c.nc, accountlifecyclev1.InventorySubject(c.name), &req, c.bound(ctx), req.AccountID)
}

func (c *natsParticipantClient) Terminate(ctx context.Context, req accountlifecyclev1.TerminateRequest) (*accountlifecyclev1.TerminateReply, error) {
	return natsmsg.NATSRequest[accountlifecyclev1.TerminateReply](ctx, c.nc, accountlifecyclev1.TerminateSubject(c.name), &req, c.bound(ctx), req.AccountID)
}

func (c *natsParticipantClient) bound(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return max(min(c.timeout, time.Until(deadline)), time.Millisecond)
	}
	return c.timeout
}

// composeParticipants builds a client for every declared participant. A
// declaration that cannot be served refuses the whole engine.
func composeParticipants(nc *nats.Conn, decls []accountlifecyclev1.Declaration) ([]Participant, error) {
	out := make([]Participant, 0, len(decls))
	for _, d := range decls {
		if err := checkDeclaration(d); err != nil {
			return nil, err
		}
		out = append(out, Participant{Declaration: d, Client: &natsParticipantClient{nc: nc, name: d.Name, timeout: participantRequestTimeout}})
	}
	return out, nil
}

func checkDeclaration(d accountlifecyclev1.Declaration) error {
	switch {
	case !accountlifecyclev1.ValidParticipantName(d.Name):
		return fmt.Errorf("participant %q: invalid name", d.Name)
	case len(d.Classes) == 0:
		return fmt.Errorf("participant %s declares no classes", d.Name)
	case d.InventoryContractVersion != accountlifecyclev1.ContractVersion || d.CommandContractVersion != accountlifecyclev1.ContractVersion:
		return fmt.Errorf("participant %s serves contract versions this engine does not speak", d.Name)
	case d.DrainBudget <= 0:
		return fmt.Errorf("participant %s declares no drain budget", d.Name)
	}
	return nil
}

// ParticipantResult is what one participant reported in a run.
type ParticipantResult struct {
	Participant string                           `json:"participant"`
	Classes     []accountlifecyclev1.ClassReport `json:"classes,omitempty"`
	Blocked     []accountlifecyclev1.BlockedItem `json:"blocked,omitempty"`
	Clean       bool                             `json:"clean"`
	Reason      string                           `json:"reason,omitempty"`
	Passes      int                              `json:"passes,omitempty"`
}

// ErrParticipantBlocked reports a participant that was not clean at the end
// of its drain budget. The account stays TERMINATING.
var ErrParticipantBlocked = errors.New("account-termination participant blocked")

// participantReplyError is a reply that breaks the contract: the wrong
// account or participant, an older declaration, or classes that do not match.
func participantReplyError(d accountlifecyclev1.Declaration, accountID, gotAccount, gotName string, gotDecl, gotContract int, classes []accountlifecyclev1.ClassReport) string {
	switch {
	case gotAccount != accountID:
		return "reply-wrong-account"
	case gotName != d.Name:
		return "reply-wrong-participant"
	case gotDecl < d.DeclarationVersion:
		return "reply-older-declaration"
	case gotContract != accountlifecyclev1.ContractVersion:
		return "reply-contract-version"
	}
	if len(classes) != len(d.Classes) {
		return "reply-class-set"
	}
	for _, c := range classes {
		if !slices.Contains(d.Classes, c.Class) {
			return "reply-class-set"
		}
		switch c.Status {
		case accountlifecyclev1.StatusClean, accountlifecyclev1.StatusPresent, accountlifecyclev1.StatusIncomplete:
		default:
			return "reply-unknown-status"
		}
	}
	seen := map[string]bool{}
	for _, c := range classes {
		if seen[c.Class] {
			return "reply-class-set"
		}
		seen[c.Class] = true
	}
	return ""
}

// readParticipant reads one participant's inventory and reports whether every
// class is clean. A reply that breaks the contract is never clean.
func readParticipant(ctx context.Context, p Participant, accountID string) ParticipantResult {
	res := ParticipantResult{Participant: p.Declaration.Name}
	reply, err := p.Client.Inventory(ctx, accountlifecyclev1.InventoryRequest{AccountID: accountID})
	if err != nil {
		res.Reason = "inventory-unanswered"
		return res
	}
	if reason := participantReplyError(p.Declaration, accountID, reply.AccountID, reply.Participant,
		reply.DeclarationVersion, reply.ContractVersion, reply.Classes); reason != "" {
		res.Reason = reason
		return res
	}
	res.Classes = reply.Classes
	res.Clean = allClean(reply.Classes)
	return res
}

func allClean(classes []accountlifecyclev1.ClassReport) bool {
	for _, c := range classes {
		if c.Status != accountlifecyclev1.StatusClean {
			return false
		}
	}
	return len(classes) > 0
}

// runParticipant repeats the command and the inventory until every class is
// clean or the participant's drain budget ends.
func (e *Engine) runParticipant(ctx context.Context, p Participant, req Request) ParticipantResult {
	deadline := e.clock()().Add(p.Declaration.DrainBudget)
	var res ParticipantResult
	for pass := 1; ; pass++ {
		res = ParticipantResult{Participant: p.Declaration.Name, Passes: pass}
		reply, err := p.Client.Terminate(ctx, accountlifecyclev1.TerminateRequest{AccountID: req.AccountID})
		switch {
		case err != nil:
			res.Reason = "command-unanswered"
		default:
			if reason := participantReplyError(p.Declaration, req.AccountID, reply.AccountID, reply.Participant,
				reply.DeclarationVersion, reply.ContractVersion, reply.Classes); reason != "" {
				res.Reason = reason
				break
			}
			res.Blocked = reply.Blocked
			read := readParticipant(ctx, p, req.AccountID)
			read.Passes, read.Blocked = pass, reply.Blocked
			res = read
		}
		if res.Clean {
			return res
		}
		if !e.clock()().Before(deadline) {
			if res.Reason == "" {
				res.Reason = "drain-budget-exhausted"
			}
			return res
		}
		if err := e.waitFn()(ctx, e.drainPoll()); err != nil {
			res.Reason = "cancelled"
			return res
		}
	}
}

// runParticipants is the participant phase: each participant in order, and a
// blocked participant stops every later one and every legacy stage.
func (e *Engine) runParticipants(ctx context.Context, req Request, result *Result) error {
	for _, p := range e.Participants {
		res := e.runParticipant(ctx, p, req)
		result.Participants = append(result.Participants, res)
		if !res.Clean {
			e.logger().ErrorContext(ctx, "Account teardown: participant blocked, account kept",
				"accountID", req.AccountID, "participant", res.Participant, "reason", res.Reason)
			return fmt.Errorf("participant %s: %s: %w", res.Participant, describeBlocked(res), ErrParticipantBlocked)
		}
	}
	return nil
}

// rereadParticipants is the final inventory read before the account record is
// deleted. Any class that is not clean keeps the account.
func (e *Engine) rereadParticipants(ctx context.Context, accountID string) error {
	for _, p := range e.Participants {
		if res := readParticipant(ctx, p, accountID); !res.Clean {
			return fmt.Errorf("participant %s on final read: %s: %w", res.Participant, describeBlocked(res), ErrParticipantBlocked)
		}
	}
	return nil
}

// inventoryParticipants reports every participant's inventory and invokes no command.
func (e *Engine) inventoryParticipants(ctx context.Context, accountID string) []ParticipantResult {
	out := make([]ParticipantResult, 0, len(e.Participants))
	for _, p := range e.Participants {
		out = append(out, readParticipant(ctx, p, accountID))
	}
	return out
}

func describeBlocked(res ParticipantResult) string {
	var parts []string
	if res.Reason != "" {
		parts = append(parts, res.Reason)
	}
	for _, c := range res.Classes {
		if c.Status != accountlifecyclev1.StatusClean {
			parts = append(parts, fmt.Sprintf("%s=%s(%d)", c.Class, c.Status, c.Remaining))
		}
	}
	for _, b := range res.Blocked {
		parts = append(parts, fmt.Sprintf("%s:%s(%d)", b.Class, b.Reason, b.Count))
	}
	if len(parts) == 0 {
		return "not clean"
	}
	return strings.Join(parts, ", ")
}

func (e *Engine) clock() func() time.Time {
	if e.now != nil {
		return e.now
	}
	return time.Now
}

func (e *Engine) waitFn() func(ctx context.Context, d time.Duration) error {
	if e.wait != nil {
		return e.wait
	}
	return func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
}
