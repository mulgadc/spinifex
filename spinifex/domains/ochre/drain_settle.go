package ochre

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre/readiness"
)

// errDrainSettlement is a drain rewrite that is not one of the two settling
// transitions, or whose record data cannot be read without ambiguity.
var errDrainSettlement = errors.New("bedrock: not a drain settlement of an endpoint record")

var _ readiness.SettleCheck = CheckDrainSettlement

// CheckDrainSettlement allows a drain token to clear the health hold of a READY
// record, or to promote a parked, complete STARTING bundle to READY. Every
// other field stays as stored; it cannot show that a health probe ran.
func CheckDrainSettlement(s readiness.Settlement) error {
	switch {
	case s.Kind != readiness.TokenDrain || s.TokenID == "" || s.Epoch == 0 || s.Rollback == "" || s.Node == "":
		return fmt.Errorf("%w: not an admitted drain token", errDrainSettlement)
	case s.Next == nil:
		return fmt.Errorf("%w: a drain never removes a record", errDrainSettlement)
	case s.Target == "" || s.Target != s.Key:
		return fmt.Errorf("%w: token target %q is not key %q", errDrainSettlement, s.Target, s.Key)
	}
	if err := checkRecovery(s.Key, s.Old, s.Next, nil); err != nil {
		return fmt.Errorf("%w: %w", errDrainSettlement, err)
	}
	return nil
}

// checkRecovery allows next to clear the hold of the READY record old, with
// a bounded fresh observation, or to promote the parked complete bundle old
// to READY. readyScope, when set, must also accept a held READY record.
func checkRecovery(key string, oldData, nextData []byte, readyScope func(EndpointRecord) error) error {
	old, err := strictRecord(oldData)
	if err != nil {
		return fmt.Errorf("stored record: %w", err)
	}
	next, err := strictRecord(nextData)
	if err != nil {
		return fmt.Errorf("new record: %w", err)
	}
	if resolveKey(resolveAccountID(old.AccountID), old.ModelID) != key {
		return fmt.Errorf("record does not belong at key %q", key)
	}
	if old.Generation == math.MaxUint64 {
		return errors.New("generation cannot advance")
	}

	want := old
	switch {
	case old.State == StateReady:
		if err := heldReady(old); err != nil {
			return err
		}
		if readyScope != nil {
			if err := readyScope(old); err != nil {
				return err
			}
		}
		if err := freshObservation(old, next); err != nil {
			return err
		}
		want.HealthHold = nil
		want.LastActiveAt, want.InFlight = next.LastActiveAt, next.InFlight
		want.SuccessTotal, want.ScrapeFailures = next.SuccessTotal, next.ScrapeFailures
	case old.State == StateStarting:
		primaryURL, err := markedBundle(old)
		if err != nil {
			return err
		}
		if next.ReadyAt.IsZero() {
			return errors.New("a promoted record needs its ready time")
		}
		want.State = StateReady
		want.ParkedReason, want.ParkedAt = "", time.Time{}
		want.BaseURL = primaryURL
		want.ReadyAt = next.ReadyAt
	default:
		return fmt.Errorf("no settlement from %s", old.State)
	}
	want.Generation = old.Generation + 1

	same, err := sameEncoding(want, next)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("new record changes more than the %s settlement allows", old.State)
	}
	return nil
}

// sameEncoding reports whether want and next encode to the same bytes.
func sameEncoding(want, next EndpointRecord) (bool, error) {
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return false, err
	}
	nextJSON, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	return bytes.Equal(wantJSON, nextJSON), nil
}

// heldReady checks that rec is a complete READY bundle on its own primary
// address, whose hold names the operation that reserved it. A legacy record
// with no launch owner qualifies.
func heldReady(rec EndpointRecord) error {
	h := rec.HealthHold
	if rec.ReadyAt.IsZero() {
		return errors.New("READY record has no ready time")
	}
	bundle, err := catalogBundle(rec)
	if err != nil {
		return err
	}
	if rec.BaseURL != bundle.targets[bundle.primary].BaseURL {
		return errors.New("base URL differs from the primary member")
	}
	switch {
	case h == nil:
		return errors.New("READY record carries no health hold")
	case h.Token == "" || h.Epoch == 0 || h.ReservedAt.IsZero():
		return errors.New("health hold has no complete token")
	case rec.ParkedReason != "" || !rec.ParkedAt.IsZero():
		return errors.New("READY record carries a park marker")
	}
	return nil
}

// freshObservation checks the probe data a hold clear may record with it:
// whole counts within the metrics bounds, a failure count kept or reset, and
// a newer activity time whenever the sample shows work.
func freshObservation(old, next EndpointRecord) error {
	switch {
	case next.InFlight < 0 || next.InFlight > maxExactCount:
		return fmt.Errorf("queue depth %d is out of range", next.InFlight)
	case next.SuccessTotal < 0 || next.SuccessTotal > maxExactCount || next.SuccessTotal != math.Trunc(next.SuccessTotal):
		return errors.New("success count is not a whole representable number")
	case next.ScrapeFailures != old.ScrapeFailures && next.ScrapeFailures != 0:
		return errors.New("a healthy observation only resets the failure count")
	case !next.LastActiveAt.Equal(old.LastActiveAt) && (next.LastActiveAt.IsZero() || next.LastActiveAt.Before(old.LastActive())):
		return errors.New("activity time moves back")
	case (next.InFlight > 0 || next.SuccessTotal != old.SuccessTotal) && !next.LastActiveAt.After(old.LastActive()):
		return errors.New("a sample showing work needs a newer activity time")
	}
	return nil
}

// markedBundle checks that rec is a parked STARTING claim with a valid launch
// owner, no observation and its complete co-serve group, and returns the
// primary's base URL.
func markedBundle(rec EndpointRecord) (string, error) {
	if _, ok, err := rec.Owner(); err != nil || !ok {
		return "", errors.New("STARTING record has no valid launch owner")
	}
	if !rec.Parked() || rec.HealthHold != nil || !rec.ReadyAt.IsZero() || !unobserved(rec) {
		return "", errors.New("STARTING record is not a parked launch")
	}
	_, primaryURL, err := parkedTargets(rec)
	if err != nil {
		return "", err
	}
	if rec.BaseURL != "" && rec.BaseURL != primaryURL {
		return "", errors.New("base URL differs from the primary member")
	}
	return primaryURL, nil
}

// strictRecord decodes data only when it is one record in the exact encoding
// this build writes: no repeated or case-alias keys, no unknown or defaulted
// fields, and a launch owner that is absent or valid.
func strictRecord(data []byte) (EndpointRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueKeys(dec); err != nil {
		return EndpointRecord{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return EndpointRecord{}, errors.New("data follows the record")
	}
	var rec EndpointRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return EndpointRecord{}, err
	}
	if _, ok, err := rec.Owner(); ok && err != nil {
		return EndpointRecord{}, err
	}
	canon, err := json.Marshal(rec)
	if err != nil {
		return EndpointRecord{}, err
	}
	same, err := sameJSON(data, canon)
	if err != nil {
		return EndpointRecord{}, err
	}
	if !same {
		return EndpointRecord{}, errors.New("record is not in its exact encoding")
	}
	return rec, nil
}

// uniqueKeys reads one JSON value from dec and refuses any object in it that
// repeats a key, in any letter case.
func uniqueKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		var seen []string
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := k.(string)
			for _, s := range seen {
				if strings.EqualFold(s, key) {
					return fmt.Errorf("repeated or case-alias key %q", key)
				}
			}
			seen = append(seen, key)
			if err := uniqueKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	case json.Delim('['):
		for dec.More() {
			if err := uniqueKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	}
	return err
}

// sameJSON reports whether a and b hold the same JSON value, with numbers
// compared as written.
func sameJSON(a, b []byte) (bool, error) {
	var va, vb any
	for _, p := range []struct {
		data []byte
		v    *any
	}{{a, &va}, {b, &vb}} {
		dec := json.NewDecoder(bytes.NewReader(p.data))
		dec.UseNumber()
		if err := dec.Decode(p.v); err != nil {
			return false, err
		}
	}
	return reflect.DeepEqual(va, vb), nil
}
