// Command rds-agent runs inside a Spinifex RDS instance; the implementation is
// in agents/rds/agent. This binary owns process lifecycle: signal handling,
// treating a cancelled startup as a clean stop, failure logging and the exit
// code.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	rdsagent "github.com/mulgadc/spinifex/spinifex/agents/rds/agent"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
)

// version is the agent build version, reported at registration.
// Overridable via -ldflags "-X main.version=...".
var version = "dev"

// engineCatalogAdapter composes the agent's narrow EngineCatalog over the
// control plane's own engine package. This binary is the only place in the
// agent's dependency graph that imports it.
type engineCatalogAdapter struct {
	engine rdsengine.Engine
}

var _ rdsagent.EngineCatalog = engineCatalogAdapter{}

func (a engineCatalogAdapter) OptionFileName(name string) string {
	return a.engine.OptionFileName(name)
}

func (a engineCatalogAdapter) LookupParameter(name string) (rdsagent.CatalogParameter, bool) {
	spec, ok := a.engine.LookupParameter(name)
	if !ok {
		return rdsagent.CatalogParameter{}, false
	}
	return rdsagent.CatalogParameter{DataType: spec.DataType, Static: spec.ApplyType == rdsengine.ApplyTypeStatic}, true
}

func (a engineCatalogAdapter) CatalogParameterNames() []string {
	return a.engine.CatalogParameterNames()
}

func (a engineCatalogAdapter) TLSEnforcementParameter() string {
	return a.engine.TLSEnforcementParameter()
}

func (a engineCatalogAdapter) ValidateMasterUsername(username string) error {
	return a.engine.ValidateMasterUsername(username)
}

func (a engineCatalogAdapter) ValidateUsernameNotReserved(username string) error {
	return a.engine.ValidateUsernameNotReserved(username)
}

func lookupEngineCatalog(name string) (rdsagent.EngineCatalog, error) {
	e, err := rdsengine.LookupEngine(name)
	if err != nil {
		return nil, err
	}
	return engineCatalogAdapter{engine: e}, nil
}

func main() {
	cfg := rdsagent.LoadConfig(rdsagent.DefaultEnvFile)
	cfg.AgentVersion = version
	cfg.EngineCatalog = lookupEngineCatalog

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	agent, err := rdsagent.Start(ctx, cfg)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		slog.Error("rds-agent: startup failed", "err", err)
		os.Exit(1)
	}

	// A shutdown signal cancels whatever boot retry loop was running; that is a
	// clean stop, not a failure.
	if err := agent.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("rds-agent: run failed", "err", err)
		os.Exit(1)
	}
}
