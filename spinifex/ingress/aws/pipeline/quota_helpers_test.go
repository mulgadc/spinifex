package pipeline

import (
	"context"
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func quotaPtr(v int) *int { return &v }

// quotaByGateway carries the quota service a test built for a gateway, which no
// longer holds one: the registration helpers read it back to compose Deps.
var quotaByGateway sync.Map

func withQuota(gw *Config, q *admissionquota.Service) *Config {
	quotaByGateway.Store(gw, q)
	return gw
}

func quotaFor(gw *Config) *admissionquota.Service {
	q, _ := quotaByGateway.Load(gw)
	svc, _ := q.(*admissionquota.Service)
	return svc
}

// quotaIAMService resolves exactly one account.
type quotaIAMService struct {
	gwfix.PolicyIAM

	known string
}

func (m *quotaIAMService) GetAccount(accountID string) (*iamdomain.Account, error) {
	if accountID != m.known {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iamdomain.Account{AccountID: accountID, Status: iamdomain.AccountStatusActive}, nil
}

// quotaTestGateway wires a real quota Service over a real JetStream KV bucket.
func quotaTestGateway(t *testing.T) (*Config, string) {
	t.Helper()
	const account = "000000000042"

	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{
		Bucket:  admissionquota.KVBucketAccountQuota,
		History: 1,
	})
	require.NoError(t, err)

	quota := admissionquota.New(admissionquota.Limits{
		Enabled: true, VCPUs: 16, VPCs: 4, Subnets: 16, EIPs: 4,
		Volumes: 16, VolumesGiB: 200, RDSInstances: 2, LoadBalancers: 2,
	}, nil)
	quota.SetOverrides(kv)

	svc := &quotaIAMService{known: account}
	return withQuota(withVerifier(gwfix.WithDeps(&Config{
		DisableLogging: true,
	}, gwfix.Deps{IAM: svc})), quota), account
}

// quotaStubChecker answers AccountTerminating from a fixed set, so a test can
// seal a record without a real IAM bucket.
type quotaStubChecker struct {
	terminating map[string]bool
}

func (c *quotaStubChecker) AccountTerminating(_ context.Context, accountID string) (bool, error) {
	return c.terminating[accountID], nil
}
