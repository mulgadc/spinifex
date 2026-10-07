package service

import (
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/services/awsgw"
	"github.com/mulgadc/spinifex/spinifex/services/nats"
	"github.com/mulgadc/spinifex/spinifex/services/northstar"
	"github.com/mulgadc/spinifex/spinifex/services/predastore"
	"github.com/mulgadc/spinifex/spinifex/services/qemunbdd"
	"github.com/mulgadc/spinifex/spinifex/services/qmpcollector"
	"github.com/mulgadc/spinifex/spinifex/services/spinifex"
	"github.com/mulgadc/spinifex/spinifex/services/spinifexui"
	"github.com/mulgadc/spinifex/spinifex/services/viperblockd"
	"github.com/mulgadc/spinifex/spinifex/vpcd"
)

// Service is a Spinifex component the CLI can launch. Start runs it and
// returns the PID written to its pid file.
type Service interface {
	Start() (int, error)
}

var (
	_ Service = (*nats.Service)(nil)
	_ Service = (*northstar.Service)(nil)
	_ Service = (*predastore.Service)(nil)
	_ Service = (*viperblockd.Service)(nil)
	_ Service = (*qemunbdd.Service)(nil)
	_ Service = (*spinifex.Service)(nil)
	_ Service = (*awsgw.Service)(nil)
	_ Service = (*spinifexui.Service)(nil)
	_ Service = (*vpcd.Service)(nil)
	_ Service = (*qmpcollector.Service)(nil)
)

// New returns the Service registered under btype (nats, viperblock, awsgw and
// so on), passing config through to its constructor. Unknown types error.
func New(btype string, config any) (Service, error) {
	switch btype {
	case "nats":
		return nats.New(config)

	case "northstar":
		return northstar.New(config)

	case "predastore":
		return predastore.New(config)

	case "viperblock":
		return viperblockd.New(config)

	case "qemunbd":
		return qemunbdd.New(config)

	case "spinifex":
		return spinifex.New(config)

	case "awsgw":
		return awsgw.New(config)

	case "spinifex-ui":
		return spinifexui.New(config)

	case "vpcd":
		return vpcd.New(config)

	case "qmp-collector":
		return qmpcollector.New(config)
	}

	return nil, fmt.Errorf("unknown service type: %s", btype)
}
