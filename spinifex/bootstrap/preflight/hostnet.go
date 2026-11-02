package preflight

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostnet"
)

// hostnetUnit is the unit whose failure this check surfaces.
const hostnetUnit = "spinifex-hostnet.service"

// hostnetOVNRemote is a variable so tests never query a real OVS.
var hostnetOVNRemote = hostnet.DefaultEnv().OVNRemote

// checkHostnet runs the check the daemon and vpcd run before they start, so a
// failed host network apply shows here, not only as a refused service start.
func checkHostnet(root string) Result {
	env := hostnet.DefaultEnv()
	env.Paths = hostnet.PathsAt(root)
	env.OVNRemote = hostnetOVNRemote
	if err := hostnet.Check(context.Background(), env); err != nil {
		return Result{Path: hostnetUnit, Kind: "hostnet", Status: Failed, Detail: err.Error()}
	}
	return Result{Path: hostnetUnit, Kind: "hostnet", Status: OK}
}
