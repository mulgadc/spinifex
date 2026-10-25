package handlers_sts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const masterKeySize = 32 // AES-256, must match iamdomain.

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
	natsConn     *nats.Conn
	js           jetstream.JetStream
	sessions     *kvstore.Store[SessionCredential]
	iamSvc       iamdomain.IAMService
	masterKey    []byte
	jwksResolver ClusterJWKSResolver
}

var (
	_ STSService               = (*STSServiceImpl)(nil)
	_ iamdomain.SessionRevoker = (*STSServiceImpl)(nil)
)

// NewSTSServiceImpl constructs an STSServiceImpl. masterKey must be the 32-byte
// key shared with IAMServiceImpl. jwksResolver resolves EKS-published JWKS
// documents for AssumeRoleWithWebIdentity; a nil resolver is rejected so a
// missing binding fails at start-up rather than as a permissive path. The
// context bounds bucket creation and the schema migration only.
func NewSTSServiceImpl(ctx context.Context, natsConn *nats.Conn, iamSvc iamdomain.IAMService, masterKey []byte, jwksResolver ClusterJWKSResolver) (*STSServiceImpl, error) {
	if natsConn == nil {
		return nil, errors.New("nil NATS connection")
	}
	if iamSvc == nil {
		return nil, errors.New("nil IAM service")
	}
	if jwksResolver == nil {
		return nil, errors.New("nil cluster JWKS resolver")
	}
	if len(masterKey) != masterKeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", masterKeySize, len(masterKey))
	}

	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}

	sessions, err := initSessionCredentialsStore(ctx, js)
	if err != nil {
		return nil, fmt.Errorf("init session credentials bucket: %w", err)
	}

	slog.Info("STS service initialized",
		"sessions_bucket", KVBucketSessionCredentials)

	return &STSServiceImpl{
		natsConn:     natsConn,
		js:           js,
		sessions:     sessions,
		iamSvc:       iamSvc,
		masterKey:    masterKey,
		jwksResolver: jwksResolver,
	}, nil
}
