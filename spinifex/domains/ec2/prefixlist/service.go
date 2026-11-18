package prefixlist

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// PrefixListService defines the read actions on the S3 prefix list.
type PrefixListService interface {
	DescribePrefixLists(ctx context.Context, input *ec2.DescribePrefixListsInput, accountID string) (*ec2.DescribePrefixListsOutput, error)
	DescribeManagedPrefixLists(ctx context.Context, input *ec2.DescribeManagedPrefixListsInput, accountID string) (*ec2.DescribeManagedPrefixListsOutput, error)
	GetManagedPrefixListEntries(ctx context.Context, input *ec2.GetManagedPrefixListEntriesInput, accountID string) (*ec2.GetManagedPrefixListEntriesOutput, error)
}

var (
	_ PrefixListService = (*ServiceImpl)(nil)
	_ PrefixListService = (*NATSPrefixListService)(nil)
)

const natsRequestTimeout = 30 * time.Second

// NATSPrefixListService forwards the prefix-list reads to the daemon.
type NATSPrefixListService struct {
	natsConn *nats.Conn
}

// NewNATSPrefixListService creates the NATS client for the prefix-list reads.
func NewNATSPrefixListService(conn *nats.Conn) PrefixListService {
	return &NATSPrefixListService{natsConn: conn}
}

func (s *NATSPrefixListService) DescribePrefixLists(ctx context.Context, input *ec2.DescribePrefixListsInput, accountID string) (*ec2.DescribePrefixListsOutput, error) {
	return natsmsg.NATSRequest[ec2.DescribePrefixListsOutput](ctx, s.natsConn, "ec2.DescribePrefixLists", input, natsRequestTimeout, accountID)
}

func (s *NATSPrefixListService) DescribeManagedPrefixLists(ctx context.Context, input *ec2.DescribeManagedPrefixListsInput, accountID string) (*ec2.DescribeManagedPrefixListsOutput, error) {
	return natsmsg.NATSRequest[ec2.DescribeManagedPrefixListsOutput](ctx, s.natsConn, "ec2.DescribeManagedPrefixLists", input, natsRequestTimeout, accountID)
}

func (s *NATSPrefixListService) GetManagedPrefixListEntries(ctx context.Context, input *ec2.GetManagedPrefixListEntriesInput, accountID string) (*ec2.GetManagedPrefixListEntriesOutput, error) {
	return natsmsg.NATSRequest[ec2.GetManagedPrefixListEntriesOutput](ctx, s.natsConn, "ec2.GetManagedPrefixListEntries", input, natsRequestTimeout, accountID)
}
