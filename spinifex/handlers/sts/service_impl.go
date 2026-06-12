package handlers_sts

import (
	"errors"
	"fmt"
	"log/slog"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

const masterKeySize = 32 // AES-256, must match handlers_iam.

// STSServiceImpl implements STS operations backed by NATS JetStream KV.
// Roles and IAM crypto are resolved through the in-process IAMService; the master key
// is shared with IAMServiceImpl and not rotated by STS.
type STSServiceImpl struct {
	natsConn       *nats.Conn
	js             nats.JetStreamContext
	sessionsBucket nats.KeyValue
	iamSvc         handlers_iam.IAMService
	masterKey      []byte
}

var _ STSService = (*STSServiceImpl)(nil)

// NewSTSServiceImpl constructs an STSServiceImpl. masterKey must be the 32-byte
// key shared with IAMServiceImpl. clusterSize sets the JetStream replication factor;
// pass 1 for single-node or test setups.
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
