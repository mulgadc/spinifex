package qmpcollector

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	liveAccount  = "123456789012"
	liveInstance = "i-0123456789abcdef0"
)

func TestThreadRunNanos(t *testing.T) {
	procRoot := t.TempDir()
	writeFile(t, filepath.Join(procRoot, "100", "schedstat"), "123456789 4000 12\n")
	writeFile(t, filepath.Join(procRoot, "101", "schedstat"), "\n")

	got, err := threadRunNanos(procRoot, 100)
	require.NoError(t, err)
	assert.Equal(t, uint64(123456789), got)

	_, err = threadRunNanos(procRoot, 101)
	require.Error(t, err)
	_, err = threadRunNanos(procRoot, 999)
	require.Error(t, err)
}

func TestReadLiveCountersSumsTapsAndSkipsAVanishedOne(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{ProcRoot: filepath.Join(root, "proc"), SysRoot: filepath.Join(root, "sys")}
	writeFile(t, filepath.Join(cfg.ProcRoot, "10", "schedstat"), "1000 0 0")
	writeFile(t, filepath.Join(cfg.ProcRoot, "11", "schedstat"), "2000 0 0")
	for tap, base := range map[string]uint64{"tapa": 100, "tapb": 1000} {
		for i, name := range []string{"rx_bytes", "tx_bytes", "rx_packets", "tx_packets"} {
			writeFile(t, filepath.Join(cfg.SysRoot, "class/net", tap, "statistics", name),
				strconv.FormatUint(base+uint64(i), 10))
		}
	}

	got, err := readLiveCounters(cfg, []int{10, 11}, []string{"tapa", "tapb", "tapgone"}, time.Unix(5, 0))
	require.NoError(t, err)
	assert.Equal(t, &liveCounters{
		at: time.Unix(5, 0), cpuNanos: 3000,
		rxBytes: 1100, txBytes: 1102, rxPackets: 1104, txPackets: 1106,
	}, got)

	_, err = readLiveCounters(cfg, []int{12}, nil, time.Unix(5, 0))
	require.Error(t, err, "a vCPU thread that cannot be read fails the sample")
}

func TestLiveRates(t *testing.T) {
	base := time.UnixMilli(1_000_000)
	prev := &liveCounters{at: base, cpuNanos: 1e9, rxBytes: 100, txBytes: 200, rxPackets: 10, txPackets: 20}
	// 600 ms elapsed, not the nominal 500: rates divide by what was measured.
	cur := &liveCounters{
		at: base.Add(600 * time.Millisecond), cpuNanos: 1e9 + 300e6,
		rxBytes: 100 + 6000, txBytes: 200 + 12000, rxPackets: 10 + 6, txPackets: 20 + 18,
	}

	got, ok := liveRates(2, prev, cur)
	require.True(t, ok)
	assert.Equal(t, int64(1_000_600), got.TimeMillis)
	// 300 ms of CPU over 600 ms across 2 vCPUs.
	assert.InDelta(t, 25.0, got.CPUPercent, 1e-9)
	assert.InDelta(t, 20000.0, got.NetworkIn, 1e-6, "tap tx is what the guest received")
	assert.InDelta(t, 10000.0, got.NetworkOut, 1e-6)
	assert.InDelta(t, 30.0, got.PacketsIn, 1e-9)
	assert.InDelta(t, 10.0, got.PacketsOut, 1e-9)

	// 7 ms of CPU, 1001 bytes and 1 packet over 600 ms.
	odd := &liveCounters{
		at: base.Add(600 * time.Millisecond), cpuNanos: 1e9 + 7e6,
		rxBytes: 100, txBytes: 200 + 1001, rxPackets: 10, txPackets: 20 + 1,
	}
	got, ok = liveRates(1, prev, odd)
	require.True(t, ok)
	assert.Equal(t, 1.17, got.CPUPercent, "CPU keeps two places")
	assert.Equal(t, 1668.0, got.NetworkIn, "bytes keep none")
	assert.Equal(t, 1.67, got.PacketsIn, "packets keep two places")

	restarted := *cur
	restarted.txPackets = 0
	_, ok = liveRates(2, cur, &restarted)
	assert.False(t, ok, "a counter that went backwards is not a rate")

	_, ok = liveRates(2, cur, cur)
	assert.False(t, ok, "no elapsed time is no rate")
}

// liveFixture is a sampler over fake procfs and sysfs whose clock the test can
// move, with real NATS between it and the test.
type liveFixture struct {
	nc      *nats.Conn
	sampler *liveSampler
	cfg     *Config

	mu     sync.Mutex
	offset time.Duration
}

func newLiveFixture(t *testing.T, account string) *liveFixture {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)
	root := t.TempDir()
	f := &liveFixture{
		nc:  nc,
		cfg: &Config{ProcRoot: filepath.Join(root, "proc"), SysRoot: filepath.Join(root, "sys")},
	}
	f.write(t, 0)

	meta := telemetryv1.GuestTelemetryMeta{
		InstanceID: liveInstance, AccountID: account, VCPUs: 1, Taps: []string{"tap0"},
	}
	f.sampler = newLiveSampler(f.cfg, nc, func() telemetryv1.GuestTelemetryMeta { return meta })
	f.sampler.threadIDs = func(telemetryv1.GuestTelemetryMeta) ([]int, error) { return []int{42}, nil }
	f.sampler.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return time.Now().Add(f.offset)
	}
	return f
}

// write sets every counter to n, so each sample sees counters that only grow.
func (f *liveFixture) write(t *testing.T, n uint64) {
	t.Helper()
	writeFile(t, filepath.Join(f.cfg.ProcRoot, "42", "schedstat"), strconv.FormatUint(n*1e6, 10)+" 0 0")
	for _, name := range []string{"rx_bytes", "tx_bytes", "rx_packets", "tx_packets"} {
		writeFile(t, filepath.Join(f.cfg.SysRoot, "class/net/tap0/statistics", name), strconv.FormatUint(n, 10))
	}
}

func (f *liveFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offset += d
}

func (f *liveFixture) running() bool {
	f.sampler.mu.Lock()
	defer f.sampler.mu.Unlock()
	return f.sampler.running
}

func (f *liveFixture) watch(t *testing.T, subject string, since int64) (telemetryv1.LiveRing, error) {
	t.Helper()
	body, err := json.Marshal(telemetryv1.LiveWatch{SinceMillis: since})
	require.NoError(t, err)
	msg, err := f.nc.Request(subject, body, time.Second)
	if err != nil {
		return telemetryv1.LiveRing{}, err
	}
	var ring telemetryv1.LiveRing
	require.NoError(t, json.Unmarshal(msg.Data, &ring))
	return ring, nil
}

func TestLiveSamplerStreamsWhileWatchedAndReplaysItsRing(t *testing.T) {
	f := newLiveFixture(t, liveAccount)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub, err := f.sampler.subscribe(ctx)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	live := make(chan *nats.Msg, 64)
	liveSub, err := f.nc.ChanSubscribe(telemetryv1.LiveSampleSubject(liveAccount, liveInstance), live)
	require.NoError(t, err)
	defer func() { _ = liveSub.Unsubscribe() }()

	assert.False(t, f.running(), "nothing samples before anyone watches")
	subject := telemetryv1.LiveWatchSubject(liveAccount, liveInstance)
	ring, err := f.watch(t, subject, 0)
	require.NoError(t, err)
	assert.Empty(t, ring.Samples, "a cold sampler has no ring yet")

	var samples []telemetryv1.LiveSample
	for n := uint64(1); len(samples) < 3; n++ {
		f.write(t, n*1000)
		select {
		case msg := <-live:
			var s telemetryv1.LiveSample
			require.NoError(t, json.Unmarshal(msg.Data, &s))
			samples = append(samples, s)
		case <-time.After(2 * time.Second):
			t.Fatal("no live sample while watched")
		}
	}
	assert.Greater(t, samples[2].TimeMillis, samples[1].TimeMillis)

	// A reconnecting viewer gets only what it has not seen.
	ring, err = f.watch(t, subject, samples[0].TimeMillis)
	require.NoError(t, err)
	require.NotEmpty(t, ring.Samples)
	for _, s := range ring.Samples {
		assert.Greater(t, s.TimeMillis, samples[0].TimeMillis)
	}

	// Once the lease lapses the sampler stops and forgets its ring.
	f.advance(liveLease + time.Second)
	require.Eventually(t, func() bool { return !f.running() }, 2*time.Second, 10*time.Millisecond)
	f.sampler.mu.Lock()
	assert.Nil(t, f.sampler.ring)
	f.sampler.mu.Unlock()
}

func TestLiveSamplerAnswersOnlyItsOwnAccount(t *testing.T) {
	f := newLiveFixture(t, liveAccount)
	sub, err := f.sampler.subscribe(t.Context())
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = f.watch(t, telemetryv1.LiveWatchSubject("210987654321", liveInstance), 0)
	require.ErrorIs(t, err, nats.ErrNoResponders, "another account's watch reaches no sampler")
	assert.False(t, f.running())
}

func TestLiveSamplerRefusesAnUnsafeSubject(t *testing.T) {
	for _, account := range []string{"", "*", "12345678901>", "1234567890.1"} {
		f := newLiveFixture(t, account)
		_, err := f.sampler.subscribe(t.Context())
		require.Error(t, err, "account %q", account)
	}
}

func TestLiveSamplerRePrimesAfterAThreadLookupFails(t *testing.T) {
	f := newLiveFixture(t, liveAccount)
	var mu sync.Mutex
	failing := true
	f.sampler.threadIDs = func(telemetryv1.GuestTelemetryMeta) ([]int, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return nil, errors.New("qmp busy")
		}
		return []int{42}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub, err := f.sampler.subscribe(ctx)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	live := make(chan *nats.Msg, 64)
	liveSub, err := f.nc.ChanSubscribe(telemetryv1.LiveSampleSubject(liveAccount, liveInstance), live)
	require.NoError(t, err)
	defer func() { _ = liveSub.Unsubscribe() }()

	_, err = f.watch(t, telemetryv1.LiveWatchSubject(liveAccount, liveInstance), 0)
	require.NoError(t, err)
	select {
	case <-live:
		t.Fatal("published without vCPU threads")
	case <-time.After(3 * livePeriod):
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	for n := uint64(1); ; n++ {
		f.write(t, n*1000)
		select {
		case <-live:
			return
		case <-time.After(livePeriod):
		}
		require.Less(t, n, uint64(20), "no live sample after QMP recovered")
	}
}
