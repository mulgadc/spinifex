package vpcd

// Phase 1 compat layer: OVN client lives in spinifex/network/ovn. These
// aliases keep existing vpcd code compiling while Phase 2 migrates callers
// to import the new package directly.

import "github.com/mulgadc/spinifex/spinifex/network/ovn"

type (
	OVNClient     = ovn.Client
	LiveOVNClient = ovn.LiveClient
	ACLSpec       = ovn.ACLSpec
)

var (
	ErrNATNotFound   = ovn.ErrNATNotFound
	NewLiveOVNClient = ovn.NewLiveClient
)
