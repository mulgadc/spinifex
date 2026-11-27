package vpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// ErrInvalidVIPRequest means a load balancer address request named a subnet
// key, CIDR or mapping token that cannot identify one reservation.
var ErrInvalidVIPRequest = errors.New("invalid load balancer address request")

// ErrIPAMInconsistent means the stored subnet row contradicts the request or
// itself, so no single reservation can be chosen from it.
var ErrIPAMInconsistent = errors.New("IPAM row inconsistent")

// ReserveVIP returns the private load balancer address held by token in the
// subnet, reserving the next free one if it holds none. The leader's row
// decides; an unknown write result is an error the next call recovers from.
func (m *IPAM) ReserveVIP(ctx context.Context, subnetId, cidrBlock, token string) (string, error) {
	prefix, err := vipRequestPrefix(subnetId, cidrBlock, token)
	if err != nil {
		return "", err
	}
	for range ipamCASAttempts {
		record, rev, err := m.leaderGet(ctx, subnetId)
		switch {
		case errors.Is(err, kvstore.ErrNotFound):
			record, rev = &IPAMRecord{SubnetId: subnetId, CidrBlock: prefix.String()}, 0
		case err != nil:
			return "", err
		}
		if err := checkVIPRow(record, subnetId, prefix); err != nil {
			return "", err
		}
		held, err := vipHeldBy(record, prefix, token)
		if err != nil || held != "" {
			return held, err
		}

		next, err := m.nextAvailableIP(record)
		if err != nil {
			return "", err
		}
		record.Allocated = append(record.Allocated, IPEntry{IP: next, Purpose: PurposeLBVIP, OwnerID: token})
		if rev == 0 {
			_, err = m.store.Create(ctx, subnetId, record)
		} else {
			_, err = m.store.CompareAndSet(ctx, subnetId, record, rev)
		}
		switch {
		case err == nil:
			slog.InfoContext(ctx, "IPAM reserved load balancer address", "subnet", subnetId, "ip", next, "owner", token)
			return next, nil
		// A create that loses after reading a delete marker can return the
		// bare replicated-stream conflict code, which the store does not map.
		case errors.Is(err, kvstore.ErrExists), errors.Is(err, kvstore.ErrConflict),
			rev == 0 && kvstore.IsRevisionConflict(err):
			continue
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("IPAM CAS exhausted for subnet %s after %d attempts", subnetId, ipamCASAttempts)
}

// vipRequestPrefix validates a request before any read and returns its
// canonical IPv4 prefix.
func vipRequestPrefix(subnetId, cidrBlock, token string) (netip.Prefix, error) {
	if !validLiteralKVKey(subnetId) {
		return netip.Prefix{}, fmt.Errorf("%w: subnet key %q is not a literal key", ErrInvalidVIPRequest, subnetId)
	}
	if token == "" {
		return netip.Prefix{}, fmt.Errorf("%w: empty mapping token", ErrInvalidVIPRequest)
	}
	prefix, err := netip.ParsePrefix(cidrBlock)
	if err != nil || !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%w: %q is not an IPv4 CIDR", ErrInvalidVIPRequest, cidrBlock)
	}
	return prefix.Masked(), nil
}

// checkVIPRow refuses a stored row for another subnet or another prefix. A
// stored pool is never rebased onto the requested CIDR.
func checkVIPRow(record *IPAMRecord, subnetId string, prefix netip.Prefix) error {
	if record.SubnetId != subnetId {
		return fmt.Errorf("%w: row for subnet %s names subnet %q", ErrIPAMInconsistent, subnetId, record.SubnetId)
	}
	stored, err := netip.ParsePrefix(record.CidrBlock)
	if err != nil || stored.Masked() != prefix {
		return fmt.Errorf("%w: subnet %s holds CIDR %q, not %s", ErrIPAMInconsistent, subnetId, record.CidrBlock, prefix)
	}
	return nil
}

// vipHeldBy returns the address token already holds, or "" if it holds none.
// Two entries for the token, or one at an unusable or shared address, leave
// no single answer, so they are refused rather than one of them chosen.
func vipHeldBy(record *IPAMRecord, prefix netip.Prefix, token string) (string, error) {
	held, matches := "", 0
	for _, entry := range record.Allocated {
		if entry.Purpose != PurposeLBVIP || entry.OwnerID != token {
			continue
		}
		matches++
		held = entry.IP
	}
	switch {
	case matches == 0:
		return "", nil
	case matches > 1:
		return "", fmt.Errorf("%w: mapping %s holds more than one address in subnet %s", ErrIPAMInconsistent, token, record.SubnetId)
	}
	addr, err := netip.ParseAddr(held)
	if err != nil || !isClaimableAddr(prefix, addr) || addr.String() != held {
		return "", fmt.Errorf("%w: mapping %s holds unusable address %q in subnet %s", ErrIPAMInconsistent, token, held, record.SubnetId)
	}
	shared := 0
	for _, entry := range record.Allocated {
		if entry.IP == held {
			shared++
		}
	}
	if shared > 1 {
		return "", fmt.Errorf("%w: address %s of mapping %s is also allocated elsewhere in subnet %s", ErrIPAMInconsistent, held, token, record.SubnetId)
	}
	return held, nil
}

// ReleaseVIP frees the load balancer address ip only while token holds it in
// the subnet, by CAS at the leader's revision. Anything else at that address
// is left alone, so a replayed or stale release cannot free a replacement.
func (m *IPAM) ReleaseVIP(ctx context.Context, subnetId, ip, token string) error {
	if !validLiteralKVKey(subnetId) {
		return fmt.Errorf("%w: subnet key %q is not a literal key", ErrInvalidVIPRequest, subnetId)
	}
	if token == "" {
		return fmt.Errorf("%w: empty mapping token", ErrInvalidVIPRequest)
	}
	if addr, err := netip.ParseAddr(ip); err != nil || !addr.Is4() || addr.String() != ip {
		return fmt.Errorf("%w: %q is not an IPv4 address", ErrInvalidVIPRequest, ip)
	}
	for range ipamCASAttempts {
		record, rev, err := m.leaderGet(ctx, subnetId)
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		i, err := vipEntryAt(record, subnetId, ip, token)
		if err != nil || i < 0 {
			return err
		}
		record.Allocated = slices.Delete(record.Allocated, i, i+1)
		_, err = m.store.CompareAndSet(ctx, subnetId, record, rev)
		switch {
		case err == nil:
			slog.InfoContext(ctx, "IPAM released load balancer address", "subnet", subnetId, "ip", ip, "owner", token)
			return nil
		case !errors.Is(err, kvstore.ErrConflict):
			return err
		}
	}
	return fmt.Errorf("IPAM CAS exhausted for subnet %s after %d attempts", subnetId, ipamCASAttempts)
}

// vipEntryAt returns the index of token's lb-vip entry at ip, or -1 if token
// does not hold ip. A row naming another subnet, or a second entry at ip, is
// ambiguous ownership and refused rather than released.
func vipEntryAt(record *IPAMRecord, subnetId, ip, token string) (int, error) {
	if record.SubnetId != subnetId {
		return -1, fmt.Errorf("%w: row for subnet %s names subnet %q", ErrIPAMInconsistent, subnetId, record.SubnetId)
	}
	found, atIP := -1, 0
	for i, entry := range record.Allocated {
		if entry.IP != ip {
			continue
		}
		atIP++
		if entry.Purpose == PurposeLBVIP && entry.OwnerID == token {
			found = i
		}
	}
	if found >= 0 && atIP > 1 {
		return -1, fmt.Errorf("%w: address %s of mapping %s has more than one entry in subnet %s", ErrIPAMInconsistent, ip, token, subnetId)
	}
	return found, nil
}
