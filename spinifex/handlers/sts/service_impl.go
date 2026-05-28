package handlers_sts

import (
	"errors"
	"fmt"
	"log/slog"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

const masterKeySize = 32 // AES-256, must match handlers_iam.

// STSServiceImpl implements STS operations using NATS JetStream KV. It
// resolves roles + IAM crypto through the in-process IAMService and persists
// session credentials in its own bucket. The master key is supplied by the
// awsgw startup path and shared by reference with IAMServiceImpl; STS does
// not own rotation.
type STSServiceImpl struct {
	natsConn       *nats.Conn
	js             nats.JetStreamContext
	sessionsBucket nats.KeyValue
	iamSvc         handlers_iam.IAMService
	masterKey      []byte
}

var _ STSService = (*STSServiceImpl)(nil)

// NewSTSServiceImpl constructs an STSServiceImpl. masterKey must be the same
// 32-byte secret IAM uses for at-rest envelope encryption; both services
// share it (see § Crypto reuse in the STS v1 plan). clusterSize sets the
// JetStream replication factor for the session bucket; pass 1 for
// single-node or test setups.
func NewSTSServiceImpl(natsConn *nats.Conn, iamSvc handlers_iam.IAMService, masterKey []byte, clusterSize int) (*STSServiceImpl, error) {
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

	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}

	sessionsBucket, err := initSessionCredentialsBucket(js, replicas)
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
