package daemon

//test:in-package — an export_test.go is in-package by definition; that is the
//whole of what it does. The tests using these live in daemon_test.

// The membership predicates, exported for the external test package. They are
// unexported in production because nothing outside the package should be
// deciding which set an instance is in, but they are worth testing directly:
// the key prefix used to answer this and could not be wrong, and these can.
var (
	OperatorStopped = operatorStopped
	RunsOn          = runsOn
)
