package accountlifecycle

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// sealCapabilityWait bounds the whole probe; shortened by the caller's
// deadline. Every configured node is probed in parallel against this one
// wait, so N nodes cost one wait, not N.
const sealCapabilityWait = 3 * time.Second

// sealCapabilityGrace is how long a node's probe keeps listening after its
// one expected responder has answered, long enough for a genuine second
// subscriber on the same node to be caught as a duplicate.
const sealCapabilityGrace = 150 * time.Millisecond

// Seal capability reasons, one per failing node. Each names a category, never
// wire error text.
const (
	sealCapabilityReasonNoReply       = "no-reply"
	sealCapabilityReasonDuplicate     = "duplicate"
	sealCapabilityReasonWrongIdentity = "wrong-identity"
	sealCapabilityReasonErrorReply    = "error-reply"
	sealCapabilityReasonMalformed     = "malformed"
	sealCapabilityReasonOldVersion    = "old-version"
	sealCapabilityReasonTransport     = "transport"
	sealCapabilityReasonInvalidNodeID = "invalid-node-id"
	sealCapabilityReasonSetUnknown    = "configured-node-set-unknown"
)

// sealCapabilityChecker reports whether every configured daemon supports the
// seal.
type sealCapabilityChecker interface {
	CheckSealCapability(ctx context.Context, correlationRef string) sealCapabilityReport
}

// sealCapabilityReport is one probe's result, one entry per configured node.
type sealCapabilityReport struct {
	Complete bool
	Nodes    []nodeSealCapability // one per configured node, sorted by node
}

// nodeSealCapability is one configured node's compatibility. Reason is ""
// when the node is compatible.
type nodeSealCapability struct {
	Node    string
	Version int    // 0 unless a valid reply arrived
	Reason  string // "" when compatible
}

// sealCapabilityClient probes every configured node's seal-capability subject
// in parallel. "Configured" is the same node set NewClusterEngine already
// receives; there is no separate liveness or membership source.
type sealCapabilityClient struct {
	nc      *nats.Conn
	nodeIDs []string
	wait    time.Duration // 3 s, shortened by the caller's deadline
	grace   time.Duration // 150 ms ResponderGrace
	subject func(node string) string
	valid   func(node string) bool
}

// sealCapabilityRequest and sealCapabilityReply are the wire shape every seal
// capability contract shares: the node asked about, the version it reports and
// an opaque correlation reference.
type sealCapabilityRequest struct {
	Node           string `json:"node"`
	CorrelationRef string `json:"correlation_ref,omitempty"`
}

type sealCapabilityReply struct {
	Node              string `json:"node"`
	CapabilityVersion int    `json:"capability_version"`
	CorrelationRef    string `json:"correlation_ref,omitempty"`
}

var _ sealCapabilityChecker = (*sealCapabilityClient)(nil)

func newSealCapabilityClient(nc *nats.Conn, nodeIDs []string) *sealCapabilityClient {
	return &sealCapabilityClient{
		nc: nc, nodeIDs: nodeIDs, wait: sealCapabilityWait, grace: sealCapabilityGrace,
		subject: ec2v1.AccountSettingsSealCapabilitySubject, valid: ec2v1.ValidSealCapabilityNode,
	}
}

// newQuotaSealCapabilityClient probes the quota override seal capability of
// every node in nodeIDs, which are the configured nodes that run awsgw.
func newQuotaSealCapabilityClient(nc *nats.Conn, nodeIDs []string) *sealCapabilityClient {
	return &sealCapabilityClient{
		nc: nc, nodeIDs: nodeIDs, wait: sealCapabilityWait, grace: sealCapabilityGrace,
		subject: admissionv1.AccountQuotaOverrideSealCapabilitySubject, valid: admissionv1.ValidSealCapabilityNode,
	}
}

// CheckSealCapability probes every configured node in parallel, bounded by
// one shared deadline, run immediately before each seal attempt. A cached or
// earlier result is never reused.
func (c *sealCapabilityClient) CheckSealCapability(ctx context.Context, correlationRef string) sealCapabilityReport {
	if len(c.nodeIDs) == 0 {
		return sealCapabilityReport{Complete: false}
	}

	wait := c.wait
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline))
	}

	nodes := append([]string(nil), c.nodeIDs...)
	sort.Strings(nodes)

	results := make([]nodeSealCapability, len(nodes))
	if ctx.Err() != nil || wait <= 0 {
		for i, node := range nodes {
			results[i] = nodeSealCapability{Node: node, Reason: sealCapabilityReasonNoReply}
		}
		return sealCapabilityReport{Complete: false, Nodes: results}
	}

	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		go func(i int, node string) {
			defer wg.Done()
			results[i] = c.checkNode(ctx, node, correlationRef, wait)
		}(i, node)
	}
	wg.Wait()

	complete := true
	for _, r := range results {
		if r.Reason != "" {
			complete = false
			break
		}
	}
	return sealCapabilityReport{Complete: complete, Nodes: results}
}

// checkNode probes one node and classifies the result. A node is compatible
// only when exactly one identified, decodable, matching, version >= 1 frame
// arrived, with no duplicate, conflict or cap hit.
func (c *sealCapabilityClient) checkNode(ctx context.Context, node, correlationRef string, wait time.Duration) nodeSealCapability {
	if !c.valid(node) {
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonInvalidNodeID}
	}

	data, err := json.Marshal(sealCapabilityRequest{Node: node, CorrelationRef: correlationRef})
	if err != nil {
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonMalformed}
	}

	frames, sum, err := natsmsg.Gather(ctx, c.nc, c.subject(node), data, natsmsg.GatherOpts{
		Timeout:            wait,
		ExpectedResponders: 1,
		Mode:               natsmsg.CollectUntilDeadline,
		ResponderGrace:     c.grace,
	})
	if err != nil {
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonTransport}
	}

	switch {
	case sum.CapHit, len(sum.ConflictNodes) > 0, sum.DuplicateFrames > 0:
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonDuplicate}
	case sum.Unidentified > 0:
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonWrongIdentity}
	case len(sum.ErrorResponders) > 0:
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonErrorReply}
	case sum.Received == 0, len(frames) == 0:
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonNoReply}
	}

	frame := frames[0]
	if frame.NodeID != node {
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonWrongIdentity}
	}

	var reply sealCapabilityReply
	if json.Unmarshal(frame.Data, &reply) != nil {
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonMalformed}
	}
	if reply.Node != node {
		return nodeSealCapability{Node: node, Reason: sealCapabilityReasonWrongIdentity}
	}
	if reply.CapabilityVersion < 1 {
		return nodeSealCapability{Node: node, Version: reply.CapabilityVersion, Reason: sealCapabilityReasonOldVersion}
	}
	return nodeSealCapability{Node: node, Version: reply.CapabilityVersion}
}

// errSealCapabilityIncomplete builds the retryable refusal from report. It
// names node IDs and reason categories only, never wire error text.
func errSealCapabilityIncomplete(report sealCapabilityReport) error {
	return errCapabilityIncomplete("EC2 account-settings seal capability", report)
}

// errQuotaSealCapabilityIncomplete is the quota override seal's refusal.
func errQuotaSealCapabilityIncomplete(report sealCapabilityReport) error {
	return errCapabilityIncomplete("quota override seal capability", report)
}

func errCapabilityIncomplete(what string, report sealCapabilityReport) error {
	detail := sealCapabilityReasonSetUnknown
	var parts []string
	for _, n := range report.Nodes {
		if n.Reason != "" {
			parts = append(parts, n.Node+"="+n.Reason)
		}
	}
	if len(parts) > 0 {
		detail = strings.Join(parts, ", ")
	}
	return awserrors.WithRetryAfter(
		awserrors.Errorf(awserrors.ErrorServiceUnavailable, "%s incomplete: %s", what, detail),
		5*time.Second,
	)
}
