package function

import (
	"context"
	"time"

	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

const metaRequestTimeout = 30 * time.Second

// Client is the gateway's view of the daemon-owned function records.
type Client interface {
	// Create binds the name and returns the KV revision.
	Create(ctx context.Context, accountID, region string, rec Record) (uint64, error)
	// Get returns the record and revision, or ErrNotFound.
	Get(ctx context.Context, accountID, region, name string) (Record, uint64, error)
	// CodeStored marks the code stored and returns the stored record and revision.
	CodeStored(ctx context.Context, accountID, region, name string, revision uint64, sha256Hex string, size int64) (Record, uint64, error)
	// RollbackCreate frees the name while the record is still storing at revision.
	RollbackCreate(ctx context.Context, accountID, region, name string, revision uint64) error
}

// NATSClient is the Client over NATS request/reply to the daemon.
type NATSClient struct {
	conn *nats.Conn
}

var _ Client = (*NATSClient)(nil)

// NewNATSClient builds a NATS-backed Client.
func NewNATSClient(conn *nats.Conn) *NATSClient { return &NATSClient{conn: conn} }

func (c *NATSClient) Create(ctx context.Context, accountID, region string, rec Record) (uint64, error) {
	resp, err := natsmsg.NATSRequest[CreateResponse](ctx, c.conn, SubjectCreate, &CreateRequest{Region: region, Record: rec}, metaRequestTimeout, accountID)
	if err != nil {
		return 0, err
	}
	return resp.Revision, nil
}

func (c *NATSClient) Get(ctx context.Context, accountID, region, name string) (Record, uint64, error) {
	resp, err := natsmsg.NATSRequest[GetResponse](ctx, c.conn, SubjectGet, &GetRequest{Region: region, FunctionName: name}, metaRequestTimeout, accountID)
	if err != nil {
		return Record{}, 0, err
	}
	if !resp.Found {
		return Record{}, 0, ErrNotFound
	}
	return resp.Record, resp.Revision, nil
}

func (c *NATSClient) CodeStored(ctx context.Context, accountID, region, name string, revision uint64, sha256Hex string, size int64) (Record, uint64, error) {
	resp, err := natsmsg.NATSRequest[CodeStoredResponse](ctx, c.conn, SubjectCodeStored,
		&CodeStoredRequest{Region: region, FunctionName: name, Revision: revision, SHA256: sha256Hex, Size: size}, metaRequestTimeout, accountID)
	if err != nil {
		return Record{}, 0, err
	}
	return resp.Record, resp.Revision, nil
}

func (c *NATSClient) RollbackCreate(ctx context.Context, accountID, region, name string, revision uint64) error {
	_, err := natsmsg.NATSRequest[RollbackCreateResponse](ctx, c.conn, SubjectRollbackCreate,
		&RollbackCreateRequest{Region: region, FunctionName: name, Revision: revision}, metaRequestTimeout, accountID)
	return err
}
