// Package hostile wraps any EBSProvider in one that misbehaves on purpose.
// It exists because the control plane's handling of a provider that fails,
// stalls, or answers wrongly is the part nothing tests, and the part that
// bites in production.
//
// Faults are derived from a seed, not drawn from a global source, so a
// failure found at hour six of a soak run replays exactly in a unit test. A
// fault injector whose output cannot be reproduced only produces bug reports
// nobody can act on.
package hostile

import (
	"context"

	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/nats-io/nats.go"
)

// Fault is what was injected into one call.
type Fault string

const (
	// FaultNone is the call passing through untouched.
	FaultNone Fault = "none"

	// FaultError fails the call without reaching the inner provider. The
	// caller is told no, and nothing happened.
	FaultError Fault = "error"

	// FaultErrorAfterWork fails the call after the inner provider has already
	// done it. This is the interesting half of the pair: the control plane is
	// told the volume was not created, and the blocks exist anyway. It is how
	// orphans are made, so it is the fault an orphan scan is the oracle for.
	FaultErrorAfterWork Fault = "error_after_work"

	// FaultLatency delays the call. Configure MaxLatency past the caller's
	// request timeout to reach the case where the caller gives up and the
	// provider is still working.
	FaultLatency Fault = "latency"

	// FaultUnavailable answers as though nothing is listening, which is the
	// condition owner-first routing treats as safe to retry elsewhere.
	FaultUnavailable Fault = "unavailable"

	// FaultLie succeeds and returns a wrong answer: a capacity, state or
	// handle that does not match what the provider holds. Error paths are
	// written deliberately; a plausible wrong answer is what corrupts
	// control-plane state quietly.
	FaultLie Fault = "lie"
)

// Config sets how often each fault fires. Rates are independent probabilities
// applied in the order the fields are declared, and a call takes the first
// fault that fires, so the sum need not be 1. All-zero is a pass-through
// provider, which is the right default: a caller must ask to be hurt.
type Config struct {
	// Seed fixes the fault sequence. Two runs with the same seed over the
	// same calls inject the same faults.
	Seed uint64

	ErrorRate          float64
	ErrorAfterWorkRate float64
	LatencyRate        float64
	UnavailableRate    float64
	LieRate            float64

	// MaxLatency bounds FaultLatency. Zero disables the delay while leaving
	// the fault recorded, which is useful for a fast test of the wiring.
	MaxLatency time.Duration
}

// Injection records one fault, with everything needed to reproduce it: the
// seed is in the provider, and Verb, Target and Sequence are the rest of the
// key the fault was derived from.
type Injection struct {
	Verb     string
	Target   string
	Sequence uint64
	Fault    Fault
	Detail   string
}

func (i Injection) String() string {
	return fmt.Sprintf("%s %s #%d: %s (%s)", i.Verb, i.Target, i.Sequence, i.Fault, i.Detail)
}

// injectedErrors is the closed set FaultError and FaultErrorAfterWork draw
// from. Every sentinel the contract defines appears, so a soak exercises each
// branch the control plane has for them.
var injectedErrors = []error{
	ebsprovider.ErrAlreadyExists,
	ebsprovider.ErrInvalidArgument,
	ebsprovider.ErrNotFound,
	ebsprovider.ErrVolumeInUse,
	ebsprovider.ErrUnsupportedCapability,
}

// Provider decorates an EBSProvider with faults.
type Provider struct {
	inner  ebsprovider.EBSProvider
	config Config

	mu         sync.Mutex
	sequences  map[string]uint64
	injections []Injection
}

var _ ebsprovider.EBSProvider = (*Provider)(nil)

// New wraps inner. A zero Config injects nothing.
func New(inner ebsprovider.EBSProvider, config Config) *Provider {
	return &Provider{inner: inner, config: config, sequences: make(map[string]uint64)}
}

// Injections returns what has been injected so far, in order.
func (p *Provider) Injections() []Injection {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Injection(nil), p.injections...)
}

// draw decides this call's fault. The key is (verb, target, per-key sequence)
// rather than a global counter, so concurrent calls on different volumes
// cannot reorder each other's faults; only repeated calls on the same target
// advance, and those are ordered by definition.
func (p *Provider) draw(verb, target string) Injection {
	return p.drawFault(verb, target, true)
}

// drawTruthful is draw for a verb that must never return a wrong answer. A
// drawn lie degrades to no fault, and the log says so, so the injection
// record never claims a lie that was not told.
func (p *Provider) drawTruthful(verb, target string) Injection {
	return p.drawFault(verb, target, false)
}

func (p *Provider) drawFault(verb, target string, allowLie bool) Injection {
	p.mu.Lock()
	key := verb + "\x00" + target
	sequence := p.sequences[key]
	p.sequences[key] = sequence + 1
	p.mu.Unlock()

	injection := Injection{Verb: verb, Target: target, Sequence: sequence, Fault: FaultNone}
	value, selector := p.hash(key, sequence)

	switch {
	case value < p.config.ErrorRate:
		injection.Fault = FaultError
	case value < p.config.ErrorRate+p.config.ErrorAfterWorkRate:
		injection.Fault = FaultErrorAfterWork
	case value < p.config.ErrorRate+p.config.ErrorAfterWorkRate+p.config.LatencyRate:
		injection.Fault = FaultLatency
	case value < p.config.ErrorRate+p.config.ErrorAfterWorkRate+p.config.LatencyRate+p.config.UnavailableRate:
		injection.Fault = FaultUnavailable
	case value < p.config.ErrorRate+p.config.ErrorAfterWorkRate+p.config.LatencyRate+p.config.UnavailableRate+p.config.LieRate:
		if !allowLie {
			return injection
		}
		injection.Fault = FaultLie
	default:
		return injection
	}

	injection.Detail = p.detail(injection.Fault, selector)
	p.mu.Lock()
	p.injections = append(p.injections, injection)
	p.mu.Unlock()
	return injection
}

// mix is the SplitMix64 finalizer. FNV alone is not good enough here: its
// output over keys that share long prefixes clusters badly enough to leave
// whole fault bands undrawable, which silently disables a fault class.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// hash turns the seed and key into a uniform draw plus an independent
// selector, so which fault fires and which flavour it takes do not correlate.
func (p *Provider) hash(key string, sequence uint64) (value float64, selector uint64) {
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(key))
	sum := mix(digest.Sum64() ^ mix(p.config.Seed+sequence))
	return float64(sum>>11) / float64(1<<53), mix(sum)
}

func (p *Provider) detail(fault Fault, selector uint64) string {
	switch fault {
	case FaultError, FaultErrorAfterWork:
		return injectedErrors[selector%uint64(len(injectedErrors))].Error()
	case FaultLatency:
		return p.latency(selector).String()
	case FaultUnavailable:
		return nats.ErrNoResponders.Error()
	case FaultLie:
		return lieFlavours[selector%uint64(len(lieFlavours))]
	default:
		return ""
	}
}

func (p *Provider) latency(selector uint64) time.Duration {
	if p.config.MaxLatency <= 0 {
		return 0
	}
	// Scaling a fraction of MaxLatency keeps the bound obvious and avoids
	// reducing an unsigned selector into a signed Duration.
	fraction := float64(selector>>11) / float64(1<<53)
	return time.Duration(fraction * float64(p.config.MaxLatency))
}

func (p *Provider) injectedError(selector uint64) error {
	return fmt.Errorf("hostile: %w", injectedErrors[selector%uint64(len(injectedErrors))])
}

// apply runs the part of a fault that happens before the inner provider. It
// reports whether the caller should stop, and with what.
func (p *Provider) apply(ctx context.Context, injection Injection) (stop bool, err error) {
	_, selector := p.hash(injection.Verb+"\x00"+injection.Target, injection.Sequence)
	switch injection.Fault {
	case FaultError:
		return true, p.injectedError(selector)
	case FaultUnavailable:
		return true, fmt.Errorf("hostile: %w", nats.ErrNoResponders)
	case FaultLatency:
		return false, sleep(ctx, p.latency(selector))
	default:
		return false, nil
	}
}

// after runs the part of a fault that happens once the inner provider has
// already done the work.
func (p *Provider) after(injection Injection, err error) error {
	if err != nil || injection.Fault != FaultErrorAfterWork {
		return err
	}
	_, selector := p.hash(injection.Verb+"\x00"+injection.Target, injection.Sequence)
	return p.injectedError(selector)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
