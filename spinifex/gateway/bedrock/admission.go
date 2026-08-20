package gateway_bedrock

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// concurrencyLimiter is a process-scoped, in-memory admission gate keyed by
// an opaque string built from (servingAccountID, modelID). It tracks an
// authoritative in-flight count per key and admits up to a caller-supplied
// capacity, rejecting immediately rather than queuing.
type concurrencyLimiter struct {
	mu       sync.Mutex
	inFlight map[string]int
}

func newConcurrencyLimiter() *concurrencyLimiter {
	return &concurrencyLimiter{inFlight: make(map[string]int)}
}

// selfHostLimiter is the single process-wide limiter shared by every
// self-host Converse/ConverseStream/InvokeModel/InvokeModelWithResponseStream
// call. It is always on: there is no operator switch, matching Bedrock's own
// lack of a "disable throttling" knob.
var selfHostLimiter = newConcurrencyLimiter()

// admissionKey builds the concurrencyLimiter key for a self-host inference
// request: servingAccountID is the resolved provisioned-throughput
// commitment's own account when a PT ARN was used, or the caller's own
// accountID for the shared GlobalAccountID-shorthand path — the same
// identity the endpoint resolver pins a serving VM on.
func admissionKey(servingAccountID, modelID string) string {
	return servingAccountID + "\x00" + modelID
}

// Acquire admits one in-flight request for key when under capacity,
// returning a release func the caller must invoke exactly once when the
// request completes. ok is false at capacity; release is then a safe no-op
// so a caller never needs to nil-check it before deferring.
func (l *concurrencyLimiter) Acquire(key string, capacity int) (release func(), ok bool) {
	l.mu.Lock()
	if l.inFlight[key] >= capacity {
		l.mu.Unlock()
		return func() {}, false
	}
	l.inFlight[key]++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.inFlight[key]--
			if l.inFlight[key] <= 0 {
				delete(l.inFlight, key)
			}
		})
	}, true
}

// slotReleasingSource wraps a converseStreamSource so the admission slot
// acquired before opening the upstream request is released exactly once,
// when the stream closes, regardless of how many times Close is called.
type slotReleasingSource struct {
	converseStreamSource

	release func()
	once    sync.Once
}

var _ converseStreamSource = (*slotReleasingSource)(nil)

func newSlotReleasingSource(inner converseStreamSource, release func()) *slotReleasingSource {
	return &slotReleasingSource{converseStreamSource: inner, release: release}
}

// Close delegates to the wrapped source first, then releases the admission
// slot exactly once so a slow/erroring inner Close still frees capacity.
func (s *slotReleasingSource) Close() error {
	err := s.converseStreamSource.Close()
	s.once.Do(s.release)
	return err
}

// slotReleasingInvokeSource is slotReleasingSource for the InvokeModelWith-
// ResponseStream path, which reframes over invokeStreamSource instead of
// converseStreamSource.
type slotReleasingInvokeSource struct {
	invokeStreamSource

	release func()
	once    sync.Once
}

var _ invokeStreamSource = (*slotReleasingInvokeSource)(nil)

func newSlotReleasingInvokeSource(inner invokeStreamSource, release func()) *slotReleasingInvokeSource {
	return &slotReleasingInvokeSource{invokeStreamSource: inner, release: release}
}

// Close mirrors slotReleasingSource.Close for the invoke-stream contract.
func (s *slotReleasingInvokeSource) Close() error {
	err := s.invokeStreamSource.Close()
	s.once.Do(s.release)
	return err
}

// admitSelfHost resolves this self-host request's admission capacity — the
// catalog's MaxConcurrency times the serving account's committed ModelUnits,
// or times 1 on the shared ON_DEMAND path (servingAccountID empty, no
// commitment to read) — and tries to acquire a slot on selfHostLimiter.
// err is ThrottlingException when the endpoint is already at capacity;
// release must be called exactly once when the request completes on success.
func admitSelfHost(ctx context.Context, provisioned *ProvisionedStore, servingAccountID, modelID string, entry catalogEntry) (release func(), err error) {
	units := int64(1)
	if servingAccountID != "" {
		units, err = committedModelUnits(ctx, provisioned, servingAccountID, modelID)
		if err != nil {
			return nil, err
		}
		if units < 1 {
			units = 1
		}
	}
	capacity := entry.MaxConcurrency * int(units)
	key := admissionKey(servingAccountID, modelID)
	release, ok := selfHostLimiter.Acquire(key, capacity)
	if !ok {
		slog.Warn("bedrock: self-host request throttled", "error_code", awserrors.ErrorThrottlingException,
			"model", modelID, "account", servingAccountID, "capacity", capacity)
		return nil, errors.New(awserrors.ErrorThrottlingException)
	}
	return release, nil
}
