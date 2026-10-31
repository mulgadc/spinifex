package systeminstance

import (
	"strings"
	"testing"

	identifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validSQSStoreInput(t *testing.T) (*SystemInstanceInput, []byte) {
	t.Helper()
	kp, err := nkeys.CreateUser()
	require.NoError(t, err)
	pub, err := kp.PublicKey()
	require.NoError(t, err)
	seed, err := kp.Seed()
	require.NoError(t, err)
	return &SystemInstanceInput{
		InstanceType: "sys.micro",
		Kind:         KindSQSStore,
		SQSStore:     &SQSStoreInput{DataVolumeID: "vol-0123456789abcdef0", ClientNKey: pub},
	}, seed
}

func TestValidateSQSStore(t *testing.T) {
	in, _ := validSQSStoreInput(t)
	require.NoError(t, in.ValidateSQSStore())

	_, seed := validSQSStoreInput(t)
	account, err := nkeys.CreateAccount()
	require.NoError(t, err)
	accountPub, err := account.PublicKey()
	require.NoError(t, err)

	cases := map[string]func(*SystemInstanceInput){
		"no kind":          func(in *SystemInstanceInput) { in.Kind = "" },
		"other kind":       func(in *SystemInstanceInput) { in.Kind = "lambda" },
		"ami boot":         func(in *SystemInstanceInput) { in.BootMode = BootAMI },
		"no store input":   func(in *SystemInstanceInput) { in.SQSStore = nil },
		"no volume":        func(in *SystemInstanceInput) { in.SQSStore.DataVolumeID = "" },
		"malformed volume": func(in *SystemInstanceInput) { in.SQSStore.DataVolumeID = "vol-XYZ" },
		"no key":           func(in *SystemInstanceInput) { in.SQSStore.ClientNKey = "" },
		"seed as key":      func(in *SystemInstanceInput) { in.SQSStore.ClientNKey = string(seed) },
		"account key":      func(in *SystemInstanceInput) { in.SQSStore.ClientNKey = accountPub },
		"lb agent env":     func(in *SystemInstanceInput) { in.LBAgentEnv = "LB_ID=x" },
		"lb ca cert":       func(in *SystemInstanceInput) { in.CACert = "cert" },
		"lb scheme":        func(in *SystemInstanceInput) { in.Scheme = "internal" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in, _ := validSQSStoreInput(t)
			mutate(in)
			err := in.ValidateSQSStore()
			require.ErrorIs(t, err, ErrInvalidSQSStoreInput)
			assert.NotContains(t, err.Error(), string(seed), "a seed must never be echoed")
		})
	}
}

func TestValidateSQSStore_VolumeIDMatchesTheGuestSerial(t *testing.T) {
	accepted := identifiers.GenerateResourceID("vol")
	in, _ := validSQSStoreInput(t)
	in.SQSStore.DataVolumeID = accepted
	require.NoError(t, in.ValidateSQSStore(), "a platform-generated volume ID is accepted")
	// The guest accepts only vol + 17 lowercase hex, the full 20-byte serial.
	assert.Len(t, strings.ReplaceAll(accepted, "-", ""), 20)

	for _, id := range []string{
		// 16 hex
		"vol-0123456789abcdef",
		// 18 hex: serial over 20 bytes
		"vol-0123456789abcdef01",
		// 8 hex
		"vol-01234567",
		// upper case
		"vol-0123456789ABCDEF0",
		// no dash
		"vol0123456789abcdef0",
		// not a volume
		"snap-0123456789abcdef0",
		// trailing newline
		"vol-0123456789abcdef0\n",
	} {
		in, _ := validSQSStoreInput(t)
		in.SQSStore.DataVolumeID = id
		require.ErrorIs(t, in.ValidateSQSStore(), ErrInvalidSQSStoreInput, id)
	}
}

func TestRefuseNonLoadBalancerKind(t *testing.T) {
	require.NoError(t, (&SystemInstanceInput{InstanceType: "sys.micro"}).RefuseNonLoadBalancerKind())
	in, _ := validSQSStoreInput(t)
	require.ErrorIs(t, in.RefuseNonLoadBalancerKind(), ErrInvalidSQSStoreInput)
	require.Error(t, (&SystemInstanceInput{SQSStore: &SQSStoreInput{}}).RefuseNonLoadBalancerKind())
}
