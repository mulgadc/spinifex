//go:build !lambdatestlink

package daemon

// Without the test link the daemon runs no Lambda code and subscribes nothing.
func (d *Daemon) startLambdaTestLink() {}
