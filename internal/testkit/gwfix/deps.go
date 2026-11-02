package gwfix

import (
	"sync"

	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/nats-io/nats.go"
)

// Deps are the collaborators a test composes a gateway from. The gateway
// config no longer carries them, so test setup keeps them beside the config.
type Deps struct {
	IAM   iamdomain.IAMService
	STS   stsdomain.STSService
	NATS  *nats.Conn
	Nodes int
}

var (
	depsMu sync.Mutex
	depsOf = map[any]*Deps{}
)

// WithDeps records d for gw and returns gw.
func WithDeps[P comparable](gw P, d Deps) P {
	depsMu.Lock()
	defer depsMu.Unlock()
	depsOf[gw] = &d
	return gw
}

// DepsOf returns the deps recorded for gw, creating an empty set when none.
func DepsOf[P comparable](gw P) *Deps {
	depsMu.Lock()
	defer depsMu.Unlock()
	d, ok := depsOf[gw]
	if !ok {
		d = &Deps{}
		depsOf[gw] = d
	}
	return d
}
