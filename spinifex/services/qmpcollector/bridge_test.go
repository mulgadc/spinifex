package qmpcollector

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// newTestBridge builds a bridge without a NATS subscription, so handle can be
// driven with a synthetic batch.
func newTestBridge() *bridge {
	return &bridge{
		meter:  otel.Meter(bridgeMeterName),
		series: make(map[string]map[string]bridgeEntry),
		gauges: make(map[string]metric.Float64ObservableGauge),
	}
}

// handleOne feeds one series through the bridge and returns the attributes it
// recorded for it.
func handleOne(t *testing.T, labels map[string]string) attribute.Set {
	t.Helper()
	b := newTestBridge()
	batch := types.TelemetryBatch{
		PeriodSeconds: 60,
		Node:          "node-1",
		Series:        []types.TelemetrySeries{{Name: "goanna_ec2_cpu_utilization", Labels: labels, Value: 12}},
	}
	data, err := json.Marshal(batch)
	require.NoError(t, err)
	b.handle(&nats.Msg{Subject: types.MetricsEC2SubjectPrefix + "i-test", Data: data})

	entries := b.series["goanna_ec2_cpu_utilization"]
	require.Len(t, entries, 1, "one series in, one series stored")
	for _, e := range entries {
		return e.attrs
	}
	return attribute.NewSet()
}

// attrValue returns the value of key in set, or "".
func attrValue(set attribute.Set, key string) string {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return v.AsString()
}

// Guest metrics carry account_id as a CloudWatch dimension. Traces name the
// same account aws.account_id, so the bridge mirrors it: one field answers for
// both signals, and the CloudWatch dimension is left as the contract it is.
func TestBridgeMirrorsTheAccountDimension(t *testing.T) {
	attrs := handleOne(t, map[string]string{
		"instance_id": "i-test",
		"account_id":  "000000000042",
		"namespace":   "AWS/EC2",
	})

	assert.Equal(t, "000000000042", attrValue(attrs, cloudwatchAccountDimension),
		"the CloudWatch dimension must survive unchanged")
	assert.Equal(t, "000000000042", attrValue(attrs, utils.AttrAccountID),
		"the account must also appear under the field traces use")
	assert.Equal(t, "node-1", attrValue(attrs, "node"))
}

// A series with no account must not gain a blank one: an empty account reads
// as an account in Kibana and would collect every unattributed guest.
func TestBridgeOmitsAnAbsentAccount(t *testing.T) {
	attrs := handleOne(t, map[string]string{"instance_id": "i-test"})

	assert.Empty(t, attrValue(attrs, utils.AttrAccountID),
		"an unattributed series must carry no account attribute")
}
