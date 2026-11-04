package spotinstance

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
)

func TestScanAccountListsActiveAndTerminalForOneAccount(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), makeSIR("sir-bbb", "i-bbb"))
	_, err := svc.CancelSpotInstanceRequests(t.Context(), &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: aws.StringSlice([]string{"sir-bbb"}),
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.PutSpotInstanceRequests(t.Context(), &PutSpotRequestsInput{Requests: []*ec2.SpotInstanceRequest{makeSIR("sir-other", "i-o")}}, "999999999999")
	require.NoError(t, err)

	scan, err := svc.ScanAccount(t.Context(), testAccountID)
	require.NoError(t, err)
	require.Len(t, scan.Active, 1)
	assert.Equal(t, "sir-aaa", scan.Active[0].RequestID)
	require.Len(t, scan.Terminal, 1)
	assert.Equal(t, "sir-bbb", scan.Terminal[0].RequestID)
}

func TestScanAccountFailsOnAnUndecodableRecord(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"))
	_, err := svc.activeKV.Put(t.Context(), kvutil.AccountKey(testAccountID, "sir-bad"), []byte("{not json"))
	require.NoError(t, err)

	scan, err := svc.ScanAccount(t.Context(), testAccountID)
	require.Error(t, err)
	assert.Nil(t, scan)
}

func TestScanAccountFailsOnARecordWithoutARequest(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.terminalKV.Put(t.Context(), kvutil.AccountKey(testAccountID, "sir-empty"), []byte(`{"account_id":"x"}`))
	require.NoError(t, err)

	_, err = svc.ScanAccount(t.Context(), testAccountID)
	require.Error(t, err)
}

func TestScanAccountOfAnEmptyAccountIsEmptyNotAnError(t *testing.T) {
	svc := setupTestService(t)
	scan, err := svc.ScanAccount(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, scan.Active)
	assert.Empty(t, scan.Terminal)
}

type failingGetKV struct {
	jetstream.KeyValue

	key string
}

func (k failingGetKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if key == k.key {
		return nil, errors.New("injected read failure")
	}
	return k.KeyValue.Get(ctx, key)
}

func TestScanAccountFailsWhenARecordCannotBeRead(t *testing.T) {
	svc := setupTestService(t)
	putSIRs(t, svc, makeSIR("sir-aaa", "i-aaa"), makeSIR("sir-bbb", "i-bbb"))
	svc.activeKV = failingGetKV{KeyValue: svc.activeKV, key: kvutil.AccountKey(testAccountID, "sir-bbb")}

	scan, err := svc.ScanAccount(t.Context(), testAccountID)
	require.Error(t, err)
	assert.Nil(t, scan)
}
