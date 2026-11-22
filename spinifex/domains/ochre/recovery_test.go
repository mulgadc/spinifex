package ochre

//test:in-package: drives the unexported parked recovery against the package's
// launch fakes and a per-member probe transport.

import (
	"context"
	stderrors "errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	recoveryIP     = "10.0.0.9"
	recoveryTenant = "000000000001"
)

// parkedBundle is a parked record for testModelID's co-serve group, carrying
// the exact members, families and ports a launch of that group records.
func parkedBundle(t *testing.T, accountID string) EndpointRecord {
	t.Helper()
	spec, found, selfHost := LookupCoServeGroup(testModelID)
	require.True(t, found && selfHost)
	require.Greater(t, len(spec.Members), 1, "recovery must be shown on a real bundle")
	inputs := make([]LaunchMemberInput, 0, len(spec.Members))
	for _, m := range spec.Members {
		inputs = append(inputs, LaunchMemberInput{ModelID: m.ModelID, Family: m.Family})
	}
	ports := assignMemberPorts(inputs)
	now := time.Now().UTC()
	rec := EndpointRecord{
		AccountID:    accountID,
		ModelID:      testModelID,
		State:        StateStarting,
		NodeID:       testNodeID,
		InstanceID:   "i-parked",
		ENIID:        "eni-parked",
		PrivateIP:    recoveryIP,
		Members:      map[string]MemberEndpoint{},
		CreatedAt:    now.Add(-time.Hour),
		ParkedReason: ParkReadinessTimedOut,
		ParkedAt:     now.Add(-time.Minute),
		Generation:   3,
	}
	for _, m := range spec.Members {
		vol := "vol-" + strconv.Itoa(ports[m.ModelID])
		rec.Members[m.ModelID] = MemberEndpoint{Port: ports[m.ModelID], WeightsVolumeID: vol, Family: m.Family}
		if m.Family == FamilyMeta {
			rec.WeightsVolumeID = vol
		}
	}
	return rec
}

type recoveryFixture struct {
	svc     *Service
	reaper  *Reaper
	harness *launchHarness
	probe   *memberProbe
	healthy atomic.Bool
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, http.StatusOK, sufficientGPU())
	f := &recoveryFixture{svc: s, harness: h}
	f.healthy.Store(true)
	f.probe = &memberProbe{answer: func(string, int) *http.Response {
		if f.healthy.Load() {
			return probeAnswer(http.StatusOK, "")
		}
		return probeAnswer(http.StatusServiceUnavailable, "")
	}}
	s.deps.HTTPClient = &http.Client{Transport: f.probe}
	f.reaper = testReaper(t, s, testNodeID, ReaperDeps{IdleTTL: time.Hour})
	return f
}

func (f *recoveryFixture) seed(t *testing.T, rec EndpointRecord) string {
	t.Helper()
	key := resolveKey(resolveAccountID(rec.AccountID), rec.ModelID)
	_, err := f.svc.store.Create(t.Context(), key, &rec)
	require.NoError(t, err)
	return key
}

func (f *recoveryFixture) current(t *testing.T, key string) EndpointRecord {
	t.Helper()
	rec, _, found, err := f.svc.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found, "recovery must never remove a record")
	return rec
}

func (f *recoveryFixture) probes() int {
	f.probe.mu.Lock()
	defer f.probe.mu.Unlock()
	n := 0
	for _, c := range f.probe.hits {
		n += c
	}
	return n
}

// assertNothingDone checks recovery neither tore down nor relaunched.
func (f *recoveryFixture) assertNothingDone(t *testing.T) {
	t.Helper()
	assertRetained(t, f.harness)
	assert.Zero(t, f.harness.launcher.launchCount.Load(), "recovery must not relaunch")
}

func assertStillParked(t *testing.T, got, seeded EndpointRecord) {
	t.Helper()
	assert.Equal(t, seeded, got, "a record recovery refuses must be left exactly as it was")
}

func TestRecoverParked_HealthyBundleIsPromoted(t *testing.T) {
	t.Parallel()
	for name, account := range map[string]string{"global": awsidentifiers.GlobalAccountID, "tenant": recoveryTenant} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRecoveryFixture(t)
			seeded := parkedBundle(t, account)
			key := f.seed(t, seeded)

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			got := f.current(t, key)
			assert.Equal(t, StateReady, got.State)
			assert.False(t, got.Parked())
			assert.Empty(t, got.ParkedReason)
			assert.True(t, got.ParkedAt.IsZero())
			assert.Equal(t, "http://"+net.JoinHostPort(recoveryIP, strconv.Itoa(vllmServePort)), got.BaseURL)
			assert.False(t, got.ReadyAt.IsZero())
			assert.Equal(t, seeded.Generation+1, got.Generation)
			assert.Equal(t, seeded.InstanceID, got.InstanceID)
			assert.Equal(t, seeded.Members, got.Members)
			assert.Equal(t, seeded.AccountID, got.AccountID)
			for modelID, m := range seeded.Members {
				assert.Equal(t, 1, f.probe.count(net.JoinHostPort(recoveryIP, strconv.Itoa(m.Port))), modelID)
			}
			f.assertNothingDone(t)
		})
	}
}

// The READY sweep stays global-only: a recovered tenant endpoint is not
// scraped or escalated by it, however its members answer.
func TestRecoverParked_TenantRecordKeepsGlobalOnlyReadySweep(t *testing.T) {
	t.Parallel()
	f := newRecoveryFixture(t)
	key := f.seed(t, parkedBundle(t, recoveryTenant))
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	promoted := f.current(t, key)
	require.Equal(t, StateReady, promoted.State)

	f.healthy.Store(false)
	for range maxScrapeFailures + 1 {
		require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	}
	assert.Equal(t, promoted, f.current(t, key))
	f.assertNothingDone(t)
}

func TestRecoverParked_UnhealthyMemberLeavesItParked(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(port string) *http.Response{
		"redirect": func(port string) *http.Response {
			if port == strconv.Itoa(teiBasePort) {
				return probeAnswer(http.StatusTemporaryRedirect, "http://"+redirectTarget+"/health")
			}
			return probeAnswer(http.StatusOK, "")
		},
		"missing Location": func(port string) *http.Response {
			if port == strconv.Itoa(vllmServePort) {
				return probeAnswer(http.StatusFound, "")
			}
			return probeAnswer(http.StatusOK, "")
		},
		"503": func(port string) *http.Response {
			if port == strconv.Itoa(teiBasePort) {
				return probeAnswer(http.StatusServiceUnavailable, "")
			}
			return probeAnswer(http.StatusOK, "")
		},
		"304": func(port string) *http.Response {
			if port == strconv.Itoa(vllmServePort) {
				return probeAnswer(http.StatusNotModified, "")
			}
			return probeAnswer(http.StatusOK, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRecoveryFixture(t)
			f.probe.answer = func(port string, _ int) *http.Response { return answer(port) }
			seeded := parkedBundle(t, awsidentifiers.GlobalAccountID)
			key := f.seed(t, seeded)

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			assertStillParked(t, f.current(t, key), seeded)
			assert.Zero(t, f.probe.count(redirectTarget), "a redirect target must receive no probe")
			f.assertNothingDone(t)
		})
	}
}

func TestRecoverParked_IncompleteOrMismatchedIdentityIsRefused(t *testing.T) {
	t.Parallel()
	first := func(rec EndpointRecord, family string) string {
		for id, m := range rec.Members {
			if m.Family == family {
				return id
			}
		}
		return ""
	}
	for name, mutate := range map[string]func(*EndpointRecord){
		"unknown reason":    func(r *EndpointRecord) { r.ParkedReason = "operator-hold" },
		"no park time":      func(r *EndpointRecord) { r.ParkedAt = time.Time{} },
		"no instance":       func(r *EndpointRecord) { r.InstanceID = "" },
		"no ENI":            func(r *EndpointRecord) { r.ENIID = "" },
		"no private IP":     func(r *EndpointRecord) { r.PrivateIP = "" },
		"no creation time":  func(r *EndpointRecord) { r.CreatedAt = time.Time{} },
		"no primary volume": func(r *EndpointRecord) { r.WeightsVolumeID = "" },
		"wrong primary vol": func(r *EndpointRecord) { r.WeightsVolumeID = "vol-other" },
		"missing member":    func(r *EndpointRecord) { delete(r.Members, first(*r, FamilyTEI)) },
		"extra member": func(r *EndpointRecord) {
			r.Members["bge-base-en-v1.5"] = MemberEndpoint{Port: 8009, WeightsVolumeID: "vol-x", Family: FamilyTEI}
		},
		"substituted member": func(r *EndpointRecord) {
			id := first(*r, FamilyTEI)
			m := r.Members[id]
			delete(r.Members, id)
			r.Members["bge-base-en-v1.5"] = m
		},
		"mismatched family": func(r *EndpointRecord) {
			id := first(*r, FamilyTEI)
			m := r.Members[id]
			m.Family = FamilyMeta
			r.Members[id] = m
		},
		"duplicate port": func(r *EndpointRecord) {
			// Both TEI members on one port: one healthy service would answer for two.
			var tei []string
			for id, m := range r.Members {
				if m.Family == FamilyTEI {
					tei = append(tei, id)
				}
			}
			m := r.Members[tei[1]]
			m.Port = r.Members[tei[0]].Port
			r.Members[tei[1]] = m
		},
		"member on the primary's port": func(r *EndpointRecord) {
			id := first(*r, FamilyTEI)
			m := r.Members[id]
			m.Port = vllmServePort
			r.Members[id] = m
		},
		"shared volume": func(r *EndpointRecord) {
			id := first(*r, FamilyTEI)
			m := r.Members[id]
			m.WeightsVolumeID = r.WeightsVolumeID
			r.Members[id] = m
		},
		"no member volume": func(r *EndpointRecord) {
			id := first(*r, FamilyTEI)
			m := r.Members[id]
			m.WeightsVolumeID = ""
			r.Members[id] = m
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRecoveryFixture(t)
			seeded := parkedBundle(t, awsidentifiers.GlobalAccountID)
			mutate(&seeded)
			key := f.seed(t, seeded)

			require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

			assertStillParked(t, f.current(t, key), seeded)
			assert.Zero(t, f.probes(), "an identity that cannot be compared is never probed")
			f.assertNothingDone(t)
		})
	}
}

func TestRecoverParked_ChangeDuringTheProbeYields(t *testing.T) {
	t.Parallel()
	f := newRecoveryFixture(t)
	seeded := parkedBundle(t, awsidentifiers.GlobalAccountID)
	key := f.seed(t, seeded)
	var once sync.Once
	var changed EndpointRecord
	f.probe.answer = func(string, int) *http.Response {
		once.Do(func() {
			rec, rev, _, err := f.svc.store.getRevision(context.Background(), key)
			if !assert.NoError(t, err) {
				return
			}
			rec.ParkedAt = rec.ParkedAt.Add(time.Second)
			_, err = f.svc.store.CompareAndSet(context.Background(), key, &rec, rev)
			assert.NoError(t, err)
			changed = rec
		})
		return probeAnswer(http.StatusOK, "")
	}

	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))

	got := f.current(t, key)
	assert.Equal(t, changed, got, "a promotion must yield to a change made after its observation")
	assert.True(t, got.Parked())
	f.assertNothingDone(t)
}

func TestRecoverParked_LostLeaseDoesNotPromote(t *testing.T) {
	t.Parallel()
	for name, closeAt := range map[string]string{"before the sweep": "before", "during the probe": "probe"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRecoveryFixture(t)
			seeded := parkedBundle(t, awsidentifiers.GlobalAccountID)
			key := f.seed(t, seeded)
			lost := make(chan struct{})
			var once sync.Once
			if closeAt == "before" {
				close(lost)
			} else {
				f.probe.answer = func(string, int) *http.Response {
					once.Do(func() { close(lost) })
					return probeAnswer(http.StatusOK, "")
				}
			}

			require.NoError(t, f.reaper.sweepOnce(t.Context(), lost))

			assertStillParked(t, f.current(t, key), seeded)
			if closeAt == "before" {
				assert.Zero(t, f.probes())
			}
			f.assertNothingDone(t)
		})
	}
}

// The launch's park write can land late. Recovery acts only on a park it can
// read, and a park write replayed after promotion cannot undo it.
func TestRecoverParked_DelayedParkWriteRacesPromotion(t *testing.T) {
	t.Parallel()
	f := newRecoveryFixture(t)
	f.healthy.Store(false)
	f.svc.deps.StartupTimeout = 50 * time.Millisecond
	release, landed := make(chan struct{}), make(chan error, 1)
	var parkWrite func() (uint64, error)
	f.svc.parkWrite = func(write func() (uint64, error)) (uint64, error) {
		parkWrite = write
		go func() {
			<-release
			_, err := write()
			landed <- err
		}()
		return 0, stderrors.New("nats: timeout")
	}

	_, err := f.svc.Ensure(t.Context(), &EnsureEndpointInput{ModelID: testModelID}, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()
	key := resolveKey(awsidentifiers.GlobalAccountID, testModelID)
	unparked := f.current(t, key)
	require.Equal(t, StateStarting, unparked.State)
	require.False(t, unparked.Parked(), "the park write is still in flight")

	f.healthy.Store(true)
	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	assert.Equal(t, unparked, f.current(t, key), "an unmarked claim is not recovered here")

	close(release)
	require.NoError(t, <-landed)
	require.True(t, f.current(t, key).Parked())

	require.NoError(t, f.reaper.sweepOnce(t.Context(), nil))
	promoted := f.current(t, key)
	require.Equal(t, StateReady, promoted.State)

	_, err = parkWrite()
	require.ErrorIs(t, err, kvstore.ErrConflict, "a replayed park write must not land over READY")
	assert.Equal(t, promoted, f.current(t, key))
	assertRetained(t, f.harness)
	assert.EqualValues(t, 1, f.harness.launcher.launchCount.Load(), "recovery must not relaunch")
}
