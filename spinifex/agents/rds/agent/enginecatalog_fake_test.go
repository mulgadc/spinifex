package agent

//test:in-package — covers the adapter tests use to reach the production
// catalog without this package importing it outside _test.go files.

import (
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// catalogFromEngine adapts a real engine-package Engine to this package's
// own narrow EngineCatalog, so tests exercise the production catalog's
// behaviour without the agent's production code importing the package that
// defines it.
type catalogFromEngine struct {
	engine rdsengine.Engine
}

var _ EngineCatalog = catalogFromEngine{}

func (c catalogFromEngine) OptionFileName(name string) string {
	return c.engine.OptionFileName(name)
}

func (c catalogFromEngine) LookupParameter(name string) (CatalogParameter, bool) {
	spec, ok := c.engine.LookupParameter(name)
	if !ok {
		return CatalogParameter{}, false
	}
	return CatalogParameter{DataType: spec.DataType, Static: spec.ApplyType == rdsengine.ApplyTypeStatic}, true
}

func (c catalogFromEngine) CatalogParameterNames() []string {
	return c.engine.CatalogParameterNames()
}

func (c catalogFromEngine) TLSEnforcementParameter() string {
	return c.engine.TLSEnforcementParameter()
}

func (c catalogFromEngine) ValidateMasterUsername(username string) error {
	return c.engine.ValidateMasterUsername(username)
}

func (c catalogFromEngine) ValidateUsernameNotReserved(username string) error {
	return c.engine.ValidateUsernameNotReserved(username)
}

// testEngineCatalogLookup is the EngineCatalogLookup tests wire into Config,
// backed by the real catalog so fixtures and golden files stay meaningful.
func testEngineCatalogLookup(name string) (EngineCatalog, error) {
	e, err := rdsengine.LookupEngine(name)
	if err != nil {
		return nil, err
	}
	return catalogFromEngine{engine: e}, nil
}

// settingsToParameters converts the engine package's own result type to the
// wire type the agent's command handlers carry, at the boundary test code
// crosses it.
func settingsToParameters(settings []rdsengine.Setting) []handlers_rds.Parameter {
	out := make([]handlers_rds.Parameter, 0, len(settings))
	for _, s := range settings {
		out = append(out, handlers_rds.Parameter{Name: s.Name, Value: s.Value})
	}
	return out
}
