// Package etcdmember implements eks-etcd-member, which runs inside an EKS K3s
// control-plane VM and removes a dead peer from the embedded etcd cluster.
package etcdmember

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
)

// Member is one etcd cluster member.
type Member struct {
	ID       uint64
	Name     string
	PeerURLs []string
}

// Client is the slice of the etcd cluster API the removal needs.
type Client interface {
	MemberList(ctx context.Context) ([]Member, error)
	MemberRemove(ctx context.Context, id uint64) error
}

// Config selects the member to remove.
type Config struct {
	PeerIP string
	SelfIP string
}

var (
	// ErrNoMatch means no member advertises the peer IP.
	ErrNoMatch = errors.New("no etcd member matches the peer IP")
	// ErrAmbiguous means more than one member advertises the peer IP.
	ErrAmbiguous = errors.New("more than one etcd member matches the peer IP")
	// ErrSelf means the peer IP is this host's own address.
	ErrSelf = errors.New("refusing to remove the local etcd member")
)

// Remove removes the single member whose peer URL host equals cfg.PeerIP.
func Remove(ctx context.Context, c Client, cfg Config) error {
	if cfg.PeerIP == "" {
		return errors.New("peer IP is required")
	}
	if cfg.PeerIP == cfg.SelfIP {
		return fmt.Errorf("%w: %s", ErrSelf, cfg.PeerIP)
	}
	members, err := c.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("list etcd members: %w", err)
	}
	var matches []Member
	for _, m := range members {
		if advertises(m, cfg.PeerIP) {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return fmt.Errorf("%w: %s", ErrNoMatch, cfg.PeerIP)
	case 1:
	default:
		return fmt.Errorf("%w: %s (%d members)", ErrAmbiguous, cfg.PeerIP, len(matches))
	}
	if err := c.MemberRemove(ctx, matches[0].ID); err != nil {
		return fmt.Errorf("remove etcd member %s: %w", matches[0].Name, err)
	}
	return nil
}

// advertises reports whether any peer URL of m has host ip. Matching the parsed
// host keeps 10.0.0.12 from matching 10.0.0.123.
func advertises(m Member, ip string) bool {
	for _, raw := range m.PeerURLs {
		u, err := url.Parse(raw)
		if err == nil && u.Hostname() == ip {
			return true
		}
	}
	return false
}

// Run removes the peer and returns the process exit code. A member that is
// already gone is success so a first-boot rerun stays idempotent.
func Run(ctx context.Context, c Client, cfg Config, out io.Writer) int {
	err := Remove(ctx, c, cfg)
	switch {
	case err == nil:
		_, _ = fmt.Fprintf(out, "removed etcd member %s\n", cfg.PeerIP)
		return 0
	case errors.Is(err, ErrNoMatch):
		_, _ = fmt.Fprintf(out, "member already absent: %s\n", cfg.PeerIP)
		return 0
	default:
		_, _ = fmt.Fprintf(out, "etcd member remove failed: %v\n", err)
		return 1
	}
}
