package daemon

// The membership predicates, exported for the external test package. They are
// unexported in production because nothing outside the package should be
// deciding which set an instance is in, but they are worth testing directly:
// the key prefix used to answer this and could not be wrong, and these can.
var (
	OperatorStopped = operatorStopped
	RunsOn          = runsOn
)
