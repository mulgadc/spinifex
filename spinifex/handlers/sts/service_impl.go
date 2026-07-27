package handlers_sts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const masterKeySize = 32 // AES-256, must match handlers_iam.

// STSServiceImpl implements STS operations backed by NATS JetStream KV.
// Roles and IAM crypto are resolved through the in-process IAMService; the master key
// is shared with IAMServiceImpl and not rotated by STS.
//
// STSService carries no context: the gateway calls it in-process off a SigV4
// request and IMDS calls it directly, and neither boundary passes one through.
// Each exported method therefore binds context.Background() for its KV work,
// which leaves the jetstream package's own 5s API timeout in force — the same
// wait the legacy KV API applied. Every helper below it takes the context as its
// leading parameter, so the day the contract gains one only those binding lines
// change. RunJanitor is the exception: it already owns a real context and
// threads it into the sweep.
type STSServiceImpl struct {
	natsConn       *nats.Conn
	js             jetstream.JetStream
	sessionsBucket jetstream.KeyValue
	iamSvc         handlers_iam.IAMService
	masterKey      []byte
}

var _ STSService = (*STSServiceImpl)(nil)

// NewSTSServiceImpl constructs an STSServiceImpl. masterKey must be the 32-byte
// key shared with IAMServiceImpl. clusterSize sets the JetStream replication factor;
// pass 1 for single-node or test setups. The context bounds bucket creation and
// the schema migration only.
func NewSTSServiceImpl(ctx context.Context, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, masterKey []byte, clusterSize int) (*STSServiceImpl, error) {
	if natsConn == nil {
		return nil, errors.New("nil NATS connection")
	}
	if iamSvc == nil {
		return nil, errors.New("nil IAM service")
	}
	if len(masterKey) != masterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", masterKeySize, len(masterKey))
	}

	replicas := max(clusterSize, 1)

	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}

	sessionsBucket, err := initSessionCredentialsBucket(ctx, js, replicas)
	if err != nil {
		return nil, fmt.Errorf("init session credentials bucket: %w", err)
	}

	slog.Info("STS service initialized",
		"sessions_bucket", KVBucketSessionCredentials,
		"replicas", replicas)

	return &STSServiceImpl{
		natsConn:       natsConn,
		js:             js,
		sessionsBucket: sessionsBucket,
		iamSvc:         iamSvc,
		masterKey:      masterKey,
	}, nil
}
