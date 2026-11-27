package vpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

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
		record, rev, err := m.store.LeaderGet(ctx, subnetId)
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
		case errors.Is(err, kvstore.ErrExists), errors.Is(err, kvstore.ErrConflict):
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
	held := ""
	for _, entry := range record.Allocated {
		if entry.Purpose != PurposeLBVIP || entry.OwnerID != token {
			continue
		}
		if held != "" {
			return "", fmt.Errorf("%w: mapping %s holds more than one address in subnet %s", ErrIPAMInconsistent, token, record.SubnetId)
		}
		held = entry.IP
	}
	if held == "" {
		return "", nil
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
