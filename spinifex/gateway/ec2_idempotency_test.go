//test:in-package — the decorator, the action table and the gateway's token
//bucket field are all unexported, and the point of these tests is that the
//table's entries are wired to the decorator.

package gateway

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/idempotency"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const idemTestAccount = "111122223333"

// responseFields returns an EC2 response's leaf path=value pairs, sorted. The
// XML builder emits elements in map order, so two renderings of one value
// differ in element order but not in content; the request id is dropped because
// it is freshly minted per response.
func responseFields(t *testing.T, doc []byte) []string {
	t.Helper()
	var fields []string
	var path []string
	decoder := xml.NewDecoder(bytes.NewReader(doc))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		switch tok := token.(type) {
		case xml.StartElement:
			path = append(path, tok.Name.Local)
			fields = append(fields, strings.Join(path, "/"))
		case xml.EndElement:
			path = path[:len(path)-1]
		case xml.CharData:
			if text := strings.TrimSpace(string(tok)); text != "" {
				fields = append(fields, strings.Join(path, "/")+"="+text)
			}
		}
	}
	fields = slices.DeleteFunc(fields, func(f string) bool {
		return strings.Contains(f, "requestId")
	})
	slices.Sort(fields)
	return fields
}

// newTokenGateway returns a gateway whose client-token bucket is already bound
// to a test JetStream, so dispatch never reaches a real NATS connection.
func newTokenGateway(t *testing.T) *GatewayConfig {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	kv, err := idempotency.OpenBucket(t.Context(), testutil.NewJetStream(t, nc), "gw-ec2-tokens", time.Minute)
	require.NoError(t, err)

	gw := &GatewayConfig{}
	// Burns the Once as it binds, so ec2ClientTokenBucket returns this bucket
	// rather than dialing.
	gw.ec2TokenOnce.Do(func() { gw.ec2TokenKV = kv })
	return gw
}

// dispatchTwice runs one action twice through the decorator with the same input
// and reports how many times the create actually ran.
func dispatchTwice[In, Out any](t *testing.T, gw *GatewayConfig, action string, input *In, result Out) (calls int, first, second []byte) {
	t.Helper()
	handler := ec2IdempotentHandler(func(ctx context.Context, in *In, gw *GatewayConfig, accountID string) (Out, error) {
		calls++
		return result, nil
	})
	first, err := handler.dispatch(action, input, gw, idemTestAccount, nil)
	require.NoError(t, err)
	second, err = handler.dispatch(action, input, gw, idemTestAccount, nil)
	require.NoError(t, err)
	return calls, first, second
}

// The bead's acceptance criterion, per converted action: the same token twice
// creates one resource and the duplicate gets the first call's response.
func TestIdempotentHandler_SameTokenCreatesOneResource(t *testing.T) {
	t.Parallel()
	tok := aws.String("tok-1")

	t.Run("CreateVolume", func(t *testing.T) {
		t.Parallel()
		gw := newTokenGateway(t)
		calls, first, second := dispatchTwice(t, gw, "CreateVolume",
			&ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: tok},
			ec2.Volume{VolumeId: aws.String("vol-1")})
		assert.Equal(t, 1, calls)
		assert.Equal(t, responseFields(t, first), responseFields(t, second))
		assert.Contains(t, string(second), "vol-1")
	})

	t.Run("CreateNetworkInterface", func(t *testing.T) {
		t.Parallel()
		gw := newTokenGateway(t)
		calls, first, second := dispatchTwice(t, gw, "CreateNetworkInterface",
			&ec2.CreateNetworkInterfaceInput{ClientToken: tok},
			ec2.CreateNetworkInterfaceOutput{
				NetworkInterface: &ec2.NetworkInterface{NetworkInterfaceId: aws.String("eni-1")},
			})
		assert.Equal(t, 1, calls)
		assert.Equal(t, responseFields(t, first), responseFields(t, second))
		assert.Contains(t, string(second), "eni-1")
	})

	// Both IDs have to survive the replay: re-creating either one leaks it.
	t.Run("CreateNatGateway", func(t *testing.T) {
		t.Parallel()
		gw := newTokenGateway(t)
		calls, first, second := dispatchTwice(t, gw, "CreateNatGateway",
			&ec2.CreateNatGatewayInput{ClientToken: tok},
			ec2.CreateNatGatewayOutput{
				NatGateway: &ec2.NatGateway{
					NatGatewayId: aws.String("nat-1"),
					NatGatewayAddresses: []*ec2.NatGatewayAddress{
						{AssociationId: aws.String("eipassoc-1")},
					},
				},
			})
		assert.Equal(t, 1, calls)
		assert.Equal(t, responseFields(t, first), responseFields(t, second))
		assert.Contains(t, string(second), "nat-1")
		assert.Contains(t, string(second), "eipassoc-1")
	})

	t.Run("CreateRouteTable", func(t *testing.T) {
		t.Parallel()
		gw := newTokenGateway(t)
		calls, first, second := dispatchTwice(t, gw, "CreateRouteTable",
			&ec2.CreateRouteTableInput{ClientToken: tok},
			ec2.CreateRouteTableOutput{
				RouteTable: &ec2.RouteTable{
					RouteTableId: aws.String("rtb-1"),
					Associations: []*ec2.RouteTableAssociation{
						{RouteTableAssociationId: aws.String("rtbassoc-1")},
					},
				},
			})
		assert.Equal(t, 1, calls)
		assert.Equal(t, responseFields(t, first), responseFields(t, second))
		assert.Contains(t, string(second), "rtb-1")
		assert.Contains(t, string(second), "rtbassoc-1")
	})

	t.Run("CreateEgressOnlyInternetGateway", func(t *testing.T) {
		t.Parallel()
		gw := newTokenGateway(t)
		calls, first, second := dispatchTwice(t, gw, "CreateEgressOnlyInternetGateway",
			&ec2.CreateEgressOnlyInternetGatewayInput{ClientToken: tok},
			ec2.CreateEgressOnlyInternetGatewayOutput{
				EgressOnlyInternetGateway: &ec2.EgressOnlyInternetGateway{
					EgressOnlyInternetGatewayId: aws.String("eigw-1"),
				},
			})
		assert.Equal(t, 1, calls)
		assert.Equal(t, responseFields(t, first), responseFields(t, second))
		assert.Contains(t, string(second), "eigw-1")
	})

	t.Run("CopyImage", func(t *testing.T) {
		t.Parallel()
		gw := newTokenGateway(t)
		calls, first, second := dispatchTwice(t, gw, "CopyImage",
			&ec2.CopyImageInput{Name: aws.String("copy"), ClientToken: tok},
			ec2.CopyImageOutput{ImageId: aws.String("ami-1")})
		assert.Equal(t, 1, calls)
		assert.Equal(t, responseFields(t, first), responseFields(t, second))
		assert.Contains(t, string(second), "ami-1")
	})

	t.Run("CreateCapacityReservation", func(t *testing.T) {
		t.Parallel()
		gw := newTokenGateway(t)
		calls, first, second := dispatchTwice(t, gw, "CreateCapacityReservation",
			&ec2.CreateCapacityReservationInput{ClientToken: tok},
			ec2.CreateCapacityReservationOutput{
				CapacityReservation: &ec2.CapacityReservation{
					CapacityReservationId: aws.String("cr-1"),
				},
			})
		assert.Equal(t, 1, calls)
		assert.Equal(t, responseFields(t, first), responseFields(t, second))
		assert.Contains(t, string(second), "cr-1")
	})
}

// Actions share one bucket, so the same token on two actions must be two
// separate pieces of work rather than one replaying the other's response.
func TestIdempotentHandler_TokensAreScopedPerAction(t *testing.T) {
	t.Parallel()
	gw := newTokenGateway(t)
	tok := aws.String("shared-token")

	volumeCalls, _, _ := dispatchTwice(t, gw, "CreateVolume",
		&ec2.CreateVolumeInput{ClientToken: tok}, ec2.Volume{VolumeId: aws.String("vol-1")})
	require.Equal(t, 1, volumeCalls)

	eigwCalls, _, second := dispatchTwice(t, gw, "CreateEgressOnlyInternetGateway",
		&ec2.CreateEgressOnlyInternetGatewayInput{ClientToken: tok},
		ec2.CreateEgressOnlyInternetGatewayOutput{
			EgressOnlyInternetGateway: &ec2.EgressOnlyInternetGateway{
				EgressOnlyInternetGatewayId: aws.String("eigw-1"),
			},
		})
	assert.Equal(t, 1, eigwCalls, "a second action still runs its own create once")
	assert.Contains(t, string(second), "eigw-1")
	assert.NotContains(t, string(second), "vol-1")
}

// One token with changed parameters is the AWS mismatch case, and the create
// must not run.
func TestIdempotentHandler_ParamMismatchIsRejected(t *testing.T) {
	t.Parallel()
	gw := newTokenGateway(t)
	calls := 0
	handler := ec2IdempotentHandler(func(ctx context.Context, in *ec2.CreateVolumeInput, gw *GatewayConfig, accountID string) (ec2.Volume, error) {
		calls++
		return ec2.Volume{VolumeId: aws.String("vol-1")}, nil
	})

	_, err := handler.dispatch("CreateVolume",
		&ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: aws.String("tok-mm")}, gw, idemTestAccount, nil)
	require.NoError(t, err)

	_, err = handler.dispatch("CreateVolume",
		&ec2.CreateVolumeInput{Size: aws.Int64(16), ClientToken: aws.String("tok-mm")}, gw, idemTestAccount, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIdempotentParameterMismatch, err.Error())
	assert.Equal(t, 1, calls, "a mismatched token must not create anything")
}

// Without a token the client did not ask for idempotency, so each call creates.
func TestIdempotentHandler_NoTokenCreatesEachTime(t *testing.T) {
	t.Parallel()
	gw := newTokenGateway(t)
	calls, _, _ := dispatchTwice(t, gw, "CreateVolume",
		&ec2.CreateVolumeInput{Size: aws.Int64(8)}, ec2.Volume{VolumeId: aws.String("vol-1")})
	assert.Equal(t, 2, calls)
}

// A create that failed was never made, so retrying the token must attempt it
// again rather than replay the failure.
func TestIdempotentHandler_FailedCreateIsRetryable(t *testing.T) {
	t.Parallel()
	gw := newTokenGateway(t)
	boom := errors.New("create failed")
	calls := 0
	handler := ec2IdempotentHandler(func(ctx context.Context, in *ec2.CreateVolumeInput, gw *GatewayConfig, accountID string) (ec2.Volume, error) {
		calls++
		if calls == 1 {
			return ec2.Volume{}, boom
		}
		return ec2.Volume{VolumeId: aws.String("vol-1")}, nil
	})
	input := &ec2.CreateVolumeInput{Size: aws.Int64(8), ClientToken: aws.String("tok-retry")}

	_, err := handler.dispatch("CreateVolume", input, gw, idemTestAccount, nil)
	require.ErrorIs(t, err, boom, "the handler's own error reaches the caller unchanged")

	out, err := handler.dispatch("CreateVolume", input, gw, idemTestAccount, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Contains(t, string(out), "vol-1")
}

// The creates that leak or cost money on a duplicate must stay wired to the
// idempotent constructor; reverting one to ec2Handler would silently drop it.
func TestEC2Actions_CreatesHonourClientToken(t *testing.T) {
	t.Parallel()
	honoured := []string{
		"CopyImage",
		"CreateCapacityReservation",
		"CreateEgressOnlyInternetGateway",
		"CreateNatGateway",
		"CreateNetworkInterface",
		"CreateRouteTable",
		"CreateVolume",
	}
	for _, action := range honoured {
		entry, ok := ec2Actions[action]
		require.True(t, ok, "%s must be a registered action", action)
		assert.True(t, entry.idempotent, "%s must honour ClientToken", action)
	}
}
