package agent

// CatalogParameter is the agent-owned shape of the control plane's knowledge
// about one parameter: enough to render, classify and read it back without
// this package depending on the catalog that defines it.
type CatalogParameter struct {
	// The catalog's data type for the setting, spelled the same as the
	// control plane's own ("integer", "real", "boolean", ...) but held here
	// as a plain string so no shared constant crosses the boundary.
	DataType string
	// Whether only a restart adopts the setting. Already customer-facing and
	// authoritative, since the API refuses ApplyMethod=immediate on one.
	Static bool
}

// EngineCatalog is exactly the control plane's per-engine metadata this
// agent consumes, kept narrow so this package does not import the catalog
// that defines it. The production implementation is composed at the
// binary's entry point and reaches here through Config.EngineCatalog.
type EngineCatalog interface {
	// The engine's startup spelling for a catalog parameter name.
	OptionFileName(name string) string
	// The catalog's own metadata for a parameter name, or false for one the
	// catalog does not carry.
	LookupParameter(name string) (CatalogParameter, bool)
	// Every name the catalog defines, for a read-back that has to translate
	// a generated file's spellings back to it.
	CatalogParameterNames() []string
	// The setting name that requires TLS of a client connection.
	TLSEnforcementParameter() string
	// The whole master-username rule: length, characters and reserved names.
	ValidateMasterUsername(username string) error
	// Only the reserved-name half, for a rotation that must not hand the
	// customer an account the engine itself relies on.
	ValidateUsernameNotReserved(username string) error
}

// EngineCatalogLookup resolves the control plane's metadata for a named
// engine. Nil in a build that wires none, which newEngine refuses rather
// than guessing at a definition.
type EngineCatalogLookup func(name string) (EngineCatalog, error)
