// Package etcdmember implements eks-etcd-member, which runs inside an EKS K3s
// control-plane VM and removes a dead peer from the embedded etcd cluster.
package etcdmember

import (
	"context"
	"errors"
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
	return errors.New("not implemented")
}
