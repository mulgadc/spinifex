package handlers_rds

import (
	"testing"

	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
)

// Test-only handles onto the two built-in engines. Resolved through LookupEngine
// rather than reaching into the engine package's own registry, which this
// package cannot do from outside it.
var (
	enginePostgres = mustLookupEngine("postgres")
	engineMariaDB  = mustLookupEngine("mariadb")
)

func mustLookupEngine(name string) rdsengine.Engine {
	engine, err := rdsengine.LookupEngine(name)
	if err != nil {
		panic(err)
	}
	return engine
}

// The value one name carries in a resolved set, exactly as the guest is handed
// it: resolvedParameterValues lowercases, which the combination checks want and
// an assertion about what is written into an option file does not.
func resolvedParameter(t *testing.T, resolved []Parameter, name string) string {
	t.Helper()
	for _, param := range resolved {
		if param.Name == name {
			return param.Value
		}
	}
	t.Fatalf("the resolved set carries no %s", name)
	return ""
}

// Converts the engine package's own Setting slice to this package's Parameter,
// for a test that calls ResolveEffectiveParameters directly rather than through
// resolveGroupParameters, which performs the same conversion in production.
func toParameters(settings []rdsengine.Setting) []Parameter {
	out := make([]Parameter, len(settings))
	for i, setting := range settings {
		out[i] = Parameter{Name: setting.Name, Value: setting.Value}
	}
	return out
}
