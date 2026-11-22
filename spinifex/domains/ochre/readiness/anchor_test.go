package readiness_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	configBucket   = "spinifex-cluster-config"
	endpointBucket = "bedrock-endpoints"
)

var firstRollout = readiness.FirstRollout{Confirmed: true}

// initEnv starts a server with the cluster-config bucket and an endpoint
// bucket already holding one record, as an existing deployment has.
func initEnv(t *testing.T) jetstream.JetStream {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	createBucket(t, js, configBucket)
	kv := createBucket(t, js, endpointBucket)
	_, err := kv.Put(t.Context(), "000000000000/model", []byte(`{"state":"READY"}`))
	require.NoError(t, err)
	return js
}

func newInit(js jetstream.JetStream) *readiness.Initializer {
	return readiness.NewInitializer(js, readiness.InitConfig{ConfigBucket: configBucket, EndpointBucket: endpointBucket})
}

func anchorEntry(t *testing.T, js jetstream.JetStream) jetstream.KeyValueEntry {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), configBucket)
	require.NoError(t, err)
	e, err := kv.Get(t.Context(), readiness.AnchorKey)
	require.NoError(t, err)
	return e
}

// markerSubjects counts the subjects under the reserved marker prefix.
func markerSubjects(t *testing.T, js jetstream.JetStream, bucket string) int {
	t.Helper()
	st, err := js.Stream(t.Context(), "KV_"+bucket)
	require.NoError(t, err)
	info, err := st.Info(t.Context(), jetstream.WithSubjectFilter("$KV."+bucket+"."+readiness.MarkerPrefix+">"))
	require.NoError(t, err)
	return len(info.State.Subjects)
}

// pubJS fails chosen publishes once: land sends the message before failing,
// as a lost acknowledgement would. It counts publishes per subject.
type pubJS struct {
	jetstream.JetStream
	mu    sync.Mutex
	match func(*nats.Msg) bool
	land  bool
	sent  map[string]int
}

func (p *pubJS) PublishMsg(ctx context.Context, m *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	p.mu.Lock()
	if p.sent == nil {
		p.sent = map[string]int{}
	}
	p.sent[m.Subject]++
	fail := p.match != nil && p.match(m)
	if fail {
		p.match = nil
	}
	p.mu.Unlock()
	if !fail {
		return p.JetStream.PublishMsg(ctx, m, opts...)
	}
	if p.land {
		if _, err := p.JetStream.PublishMsg(ctx, m, opts...); err != nil {
			return nil, err
		}
	}
	return nil, context.DeadlineExceeded
}

func (p *pubJS) count(prefix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for subj, c := range p.sent {
		if strings.HasPrefix(subj, prefix) {
			n += c
		}
	}
	return n
}

func TestInit_FirstRolloutBindsExistingDeployment(t *testing.T) {
	js := initEnv(t)
	a, err := newInit(js).Initialize(t.Context(), firstRollout)
	require.NoError(t, err)
	require.Equal(t, readiness.AnchorBound, a.Phase)
	require.Len(t, a.Bound, 2)

	b, err := newInit(js).Load(t.Context())
	require.NoError(t, err, "another daemon attaches under the stored anchor")
	assert.Equal(t, a, b.Anchor)
	require.NoError(t, b.Endpoints.Verify(t.Context()))
	require.NoError(t, b.Control.Verify(t.Context()))
	entry, err := b.EndpointsKV.Get(t.Context(), "000000000000/model")
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":"READY"}`, string(entry.Value()), "the existing record is preserved")
	keys, err := b.EndpointsKV.Keys(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"000000000000/model"}, keys, "the delete marker is not listed")
	for _, bucket := range []string{endpointBucket, readiness.KVBucket} {
		assert.Equal(t, 1, markerSubjects(t, js, bucket))
	}

	before := anchorEntry(t, js)
	_, err = newInit(js).Initialize(t.Context(), firstRollout)
	require.ErrorIs(t, err, readiness.ErrWrongState, "a second initialization never rebinds")
	assert.Equal(t, before.Revision(), anchorEntry(t, js).Revision())
}

func TestInit_RequiresExplicitFirstRolloutEvidence(t *testing.T) {
	js := initEnv(t)
	_, err := newInit(js).Initialize(t.Context(), readiness.FirstRollout{})
	require.ErrorIs(t, err, readiness.ErrNotFirstRollout)
	_, err = js.KeyValue(t.Context(), readiness.KVBucket)
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "nothing is created without the evidence")
	_, err = newInit(js).Load(t.Context())
	require.ErrorIs(t, err, readiness.ErrAbsent)
}

func TestInit_SurvivingEvidenceBlocksAfterAnchorLoss(t *testing.T) {
	loseAnchor := func(t *testing.T, js jetstream.JetStream) {
		require.NoError(t, js.DeleteKeyValue(t.Context(), configBucket))
		createBucket(t, js, configBucket)
	}
	cases := map[string]func(*testing.T, jetstream.JetStream){
		"control stream survives": loseAnchor,
		"only the endpoint marker survives": func(t *testing.T, js jetstream.JetStream) {
			loseAnchor(t, js)
			require.NoError(t, js.DeleteKeyValue(t.Context(), readiness.KVBucket))
		},
		"anchor deleted in place": func(t *testing.T, js jetstream.JetStream) {
			kv, err := js.KeyValue(t.Context(), configBucket)
			require.NoError(t, err)
			require.NoError(t, kv.Delete(t.Context(), readiness.AnchorKey))
		},
	}
	for name, lose := range cases {
		t.Run(name, func(t *testing.T) {
			js := initEnv(t)
			_, err := newInit(js).Initialize(t.Context(), firstRollout)
			require.NoError(t, err)
			lose(t, js)

			_, err = newInit(js).Initialize(t.Context(), firstRollout)
			require.ErrorIs(t, err, readiness.ErrPriorBinding, "explicit evidence does not override surviving evidence")
			_, err = newInit(js).Load(t.Context())
			require.Error(t, err)
		})
	}
	t.Run("control stream without any anchor", func(t *testing.T) {
		js := initEnv(t)
		createBucket(t, js, readiness.KVBucket)
		_, err := newInit(js).Initialize(t.Context(), firstRollout)
		require.ErrorIs(t, err, readiness.ErrPriorBinding)
	})
}

func TestInit_MarkerDiscoveryFindsAnyReservedSubject(t *testing.T) {
	for name, storage := range map[string]jetstream.StorageType{"file": jetstream.FileStorage, "memory": jetstream.MemoryStorage} {
		t.Run(name, func(t *testing.T) {
			_, _, js := testutil.StartTestJetStream(t)
			createBucket(t, js, configBucket)
			kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: endpointBucket, History: 1, Storage: storage})
			require.NoError(t, err)
			require.NoError(t, kv.Delete(t.Context(), readiness.MarkerPrefix+"0f1e2d3c"))
			_, err = kv.Put(t.Context(), "000000000000/model", []byte("{}"))
			require.NoError(t, err)

			_, err = newInit(js).Initialize(t.Context(), firstRollout)
			require.ErrorIs(t, err, readiness.ErrPriorBinding)
			require.ErrorContains(t, err, readiness.MarkerPrefix+"0f1e2d3c")
		})
	}
}

// stepper runs Initialize in the background and stops it at each named step.
type stepper struct {
	at      chan string
	release chan struct{}
	done    chan error
}

func pauseAt(t *testing.T, in *readiness.Initializer, steps ...string) *stepper {
	t.Helper()
	p := &stepper{at: make(chan string), release: make(chan struct{}), done: make(chan error, 1)}
	readiness.SetInitStepHook(in, func(s string) {
		for _, want := range steps {
			if s == want {
				p.at <- s
				<-p.release
			}
		}
	})
	go func() {
		_, err := in.Initialize(t.Context(), firstRollout)
		p.done <- err
	}()
	p.wait(t, steps[0])
	return p
}

// wait requires the initializer to stop at step next.
func (p *stepper) wait(t *testing.T, step string) {
	t.Helper()
	select {
	case s := <-p.at:
		require.Equal(t, step, s)
	case err := <-p.done:
		t.Fatalf("initializer finished before %q: %v", step, err)
	}
}

// next releases the current stop and waits for step.
func (p *stepper) next(t *testing.T, step string) {
	t.Helper()
	p.release <- struct{}{}
	p.wait(t, step)
}

// finish releases the current stop and returns Initialize's result.
func (p *stepper) finish() error {
	p.release <- struct{}{}
	return <-p.done
}

func TestInit_ConcurrentInitializersBindOnce(t *testing.T) {
	assertBoundOnce := func(t *testing.T, js jetstream.JetStream, before jetstream.KeyValueEntry) {
		t.Helper()
		after := anchorEntry(t, js)
		assert.Equal(t, before.Revision(), after.Revision())
		assert.Equal(t, before.Value(), after.Value())
		_, err := newInit(js).Load(t.Context())
		require.NoError(t, err)
		for _, bucket := range []string{endpointBucket, readiness.KVBucket} {
			assert.Equal(t, 1, markerSubjects(t, js, bucket), "the losing initializer wrote no marker")
		}
	}
	t.Run("loser checks after the winner binds", func(t *testing.T) {
		js := initEnv(t)
		slow := pauseAt(t, newInit(js), "evidence")
		_, err := newInit(js).Initialize(t.Context(), firstRollout)
		require.NoError(t, err)
		before := anchorEntry(t, js)
		require.ErrorIs(t, slow.finish(), readiness.ErrPriorBinding, "the winner's control marker is seen")
		assertBoundOnce(t, js, before)
	})
	t.Run("both pass every check", func(t *testing.T) {
		js := initEnv(t)
		first := pauseAt(t, newInit(js), "evidence", "pending")
		second := pauseAt(t, newInit(js), "evidence", "pending")
		first.next(t, "pending")
		second.next(t, "pending")
		require.NoError(t, first.finish())
		before := anchorEntry(t, js)
		err := second.finish()
		require.ErrorIs(t, err, readiness.ErrContended)
		require.ErrorContains(t, err, "another initializer wrote the anchor", "the anchor create decides the race")
		assertBoundOnce(t, js, before)
	})
}

// The control bucket is opened through the shared get-or-create path, which
// cannot say who created it. Ownership is proved instead by this attempt's
// marker landing as the control stream's first message.
func TestInit_ControlBucketCreatedAfterTheEvidenceCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		at      string
		history uint8
		write   bool
		want    error
		pending bool
	}{
		"empty and alike":           {at: "evidence", history: 1},
		"already written":           {at: "evidence", history: 1, write: true, want: readiness.ErrPriorBinding},
		"foreign config":            {at: "evidence", history: 5, want: readiness.ErrPriorBinding},
		"written before the marker": {at: "publish " + readiness.KVBucket, history: 1, write: true, want: readiness.ErrChanged, pending: true},
	} {
		t.Run(name, func(t *testing.T) {
			js := initEnv(t)
			in := newInit(js)
			readiness.SetInitStepHook(in, func(step string) {
				if step != tc.at {
					return
				}
				kv, err := js.KeyValue(t.Context(), readiness.KVBucket)
				if errors.Is(err, jetstream.ErrBucketNotFound) {
					kv, err = js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: readiness.KVBucket, History: tc.history})
				}
				require.NoError(t, err)
				if tc.write {
					_, err = kv.Put(t.Context(), "control", []byte(`{}`))
					require.NoError(t, err)
				}
			})
			a, err := in.Initialize(t.Context(), firstRollout)
			if tc.want == nil {
				require.NoError(t, err)
				assert.Equal(t, uint64(1), a.Bound[1].Marker.Seq, "the marker is the control stream's first message")
				_, err = newInit(js).Load(t.Context())
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
			_, err = newInit(js).Load(t.Context())
			if tc.pending {
				require.ErrorIs(t, err, readiness.ErrBindingPending, "a control stream this attempt does not own is never bound")
				assert.Zero(t, markerSubjects(t, js, readiness.KVBucket))
			} else {
				require.ErrorIs(t, err, readiness.ErrAbsent, "refused before any intent was written")
			}
		})
	}
}

func TestInit_ConfigBucketMustNotBeABoundStream(t *testing.T) {
	js := initEnv(t)
	for _, cfg := range []readiness.InitConfig{
		{ConfigBucket: endpointBucket, EndpointBucket: endpointBucket},
		{ConfigBucket: readiness.KVBucket, EndpointBucket: endpointBucket},
		{ConfigBucket: configBucket, EndpointBucket: readiness.KVBucket},
		{ConfigBucket: "", EndpointBucket: endpointBucket},
		{ConfigBucket: configBucket, EndpointBucket: ""},
	} {
		in := readiness.NewInitializer(js, cfg)
		_, err := in.Initialize(t.Context(), firstRollout)
		require.ErrorIs(t, err, readiness.ErrInvalid, "%+v", cfg)
		_, err = in.Load(t.Context())
		require.ErrorIs(t, err, readiness.ErrInvalid, "%+v", cfg)
	}
	assert.Zero(t, markerSubjects(t, js, endpointBucket))
}

func TestInit_StoredMarkerKeysMustBeExactReservedKeys(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"wildcard":      func(string) string { return readiness.MarkerPrefix + ">" },
		"token":         func(string) string { return readiness.MarkerPrefix + "*" },
		"longer suffix": func(k string) string { return k + ".x" },
		"upper case":    func(k string) string { return strings.ToUpper(k[:len(readiness.MarkerPrefix)]) + k[len(readiness.MarkerPrefix):] },
		"short suffix":  func(k string) string { return k[:len(k)-1] },
		"bare prefix":   func(string) string { return readiness.MarkerPrefix },
	} {
		t.Run(name, func(t *testing.T) {
			js := initEnv(t)
			a, err := newInit(js).Initialize(t.Context(), firstRollout)
			require.NoError(t, err)
			key := edit(a.Streams[0].MarkerKey)
			a.Streams[0].MarkerKey, a.Bound[0].Marker.Key = key, key
			data, err := json.Marshal(a)
			require.NoError(t, err)
			kv, err := js.KeyValue(t.Context(), configBucket)
			require.NoError(t, err)
			_, err = kv.Put(t.Context(), readiness.AnchorKey, data)
			require.NoError(t, err)

			_, err = newInit(js).Load(t.Context())
			require.ErrorIs(t, err, readiness.ErrUnsupported)
		})
	}
}

func TestInit_UnknownMarkerPublishStaysPending(t *testing.T) {
	for name, land := range map[string]bool{"landed": true, "not landed": false} {
		t.Run(name, func(t *testing.T) {
			js := initEnv(t)
			wrapped := &pubJS{JetStream: js, land: land, match: func(m *nats.Msg) bool {
				return strings.HasPrefix(m.Subject, "$KV."+endpointBucket+"."+readiness.MarkerPrefix)
			}}
			_, err := newInit(wrapped).Initialize(t.Context(), firstRollout)
			require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)

			_, err = newInit(wrapped).Initialize(t.Context(), firstRollout)
			require.ErrorIs(t, err, readiness.ErrBindingPending, "an uncertain publish is never retried")
			_, err = newInit(wrapped).Load(t.Context())
			require.ErrorIs(t, err, readiness.ErrBindingPending, "nor adopted")
			assert.Equal(t, 1, wrapped.count("$KV."+endpointBucket+"."+readiness.MarkerPrefix))
			assert.Zero(t, wrapped.count("$KV."+readiness.KVBucket+"."), "the next marker is not attempted")
			want := 0
			if land {
				want = 1
			}
			assert.Equal(t, want, markerSubjects(t, js, endpointBucket))
		})
	}
}

func TestInit_ReplacementBetweenIntentAndPublishIsRefused(t *testing.T) {
	js := initEnv(t)
	in := newInit(js)
	readiness.SetInitStepHook(in, func(step string) {
		if step == "publish "+endpointBucket {
			recreate(t, js, endpointBucket)
		}
	})
	_, err := in.Initialize(t.Context(), firstRollout)
	require.ErrorIs(t, err, readiness.ErrStreamReplaced)
	assert.Equal(t, 1, markerSubjects(t, js, endpointBucket), "the zero-revision write landed on the replacement")

	_, err = newInit(js).Load(t.Context())
	require.ErrorIs(t, err, readiness.ErrBindingPending, "the replacement is never bound")
}

func TestInit_LostBindingAcknowledgement(t *testing.T) {
	boundAnchor := func(m *nats.Msg) bool {
		return m.Subject == "$KV."+configBucket+"."+readiness.AnchorKey && strings.Contains(string(m.Data), `"phase":"bound"`)
	}
	t.Run("binding landed", func(t *testing.T) {
		js := initEnv(t)
		wrapped := &pubJS{JetStream: js, land: true, match: boundAnchor}
		_, err := newInit(wrapped).Initialize(t.Context(), firstRollout)
		require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)

		b, err := newInit(js).Load(t.Context())
		require.NoError(t, err, "a landed binding is adopted once both streams verify")
		assert.Equal(t, readiness.AnchorBound, b.Anchor.Phase)
		_, err = newInit(js).Initialize(t.Context(), firstRollout)
		require.ErrorIs(t, err, readiness.ErrWrongState)
	})
	t.Run("binding not landed", func(t *testing.T) {
		js := initEnv(t)
		wrapped := &pubJS{JetStream: js, match: boundAnchor}
		_, err := newInit(wrapped).Initialize(t.Context(), firstRollout)
		require.ErrorIs(t, err, readiness.ErrOutcomeUnknown)

		_, err = newInit(wrapped).Load(t.Context())
		require.ErrorIs(t, err, readiness.ErrBindingPending)
		_, err = newInit(wrapped).Initialize(t.Context(), firstRollout)
		require.ErrorIs(t, err, readiness.ErrBindingPending, "the uncertain binding write is not retried")
		assert.Equal(t, 2, wrapped.count("$KV."+configBucket+"."+readiness.AnchorKey), "one pending write and one binding attempt")
	})
}

func TestInit_UnreadableAnchorBlocks(t *testing.T) {
	for name, data := range map[string]string{
		"newer version": `{"version":2}`,
		"unknown field": `{"version":1,"phase":"bound","intent":"x","streams":[],"extra":true}`,
		"not json":      `bound`,
	} {
		t.Run(name, func(t *testing.T) {
			js := initEnv(t)
			kv, err := js.KeyValue(t.Context(), configBucket)
			require.NoError(t, err)
			_, err = kv.Put(t.Context(), readiness.AnchorKey, []byte(data))
			require.NoError(t, err)

			_, err = newInit(js).Load(t.Context())
			require.ErrorIs(t, err, readiness.ErrUnsupported)
			_, err = newInit(js).Initialize(t.Context(), firstRollout)
			require.ErrorIs(t, err, readiness.ErrUnsupported)
			assert.Equal(t, data, string(anchorEntry(t, js).Value()), "an unreadable anchor is never rewritten")
		})
	}
}
