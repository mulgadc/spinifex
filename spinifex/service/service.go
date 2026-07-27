package service

import (
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/services/awsgw"
	"github.com/mulgadc/spinifex/spinifex/services/nats"
	"github.com/mulgadc/spinifex/spinifex/services/northstar"
	"github.com/mulgadc/spinifex/spinifex/services/predastore"
	"github.com/mulgadc/spinifex/spinifex/services/qmpcollector"
	"github.com/mulgadc/spinifex/spinifex/services/spinifex"
	"github.com/mulgadc/spinifex/spinifex/services/spinifexui"
	"github.com/mulgadc/spinifex/spinifex/services/viperblockd"
	"github.com/mulgadc/spinifex/spinifex/vpcd"
)

type Service interface {
	Start() (int, error)
	Stop() error
	Status() (string, error)
	Shutdown() error
	Reload() error
}

var (
	_ Service = (*nats.Service)(nil)
	_ Service = (*northstar.Service)(nil)
	_ Service = (*predastore.Service)(nil)
	_ Service = (*viperblockd.Service)(nil)
	_ Service = (*spinifex.Service)(nil)
	_ Service = (*awsgw.Service)(nil)
	_ Service = (*spinifexui.Service)(nil)
	_ Service = (*vpcd.Service)(nil)
	_ Service = (*qmpcollector.Service)(nil)
)

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
