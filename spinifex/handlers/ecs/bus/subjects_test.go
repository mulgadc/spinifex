package bus

import "testing"

func TestSubjectBuilders(t *testing.T) {
	const (
		acct    = "123456789012"
		cluster = "default"
		inst    = "i-abc123"
		task    = "t-deadbeef"
	)
	cases := map[string]struct{ got, want string }{
		"prefix":    {Prefix(acct, cluster), "ecs.bus.123456789012.default"},
		"register":  {RegisterSubject(acct, cluster, inst), "ecs.bus.123456789012.default.instance-register.i-abc123"},
		"heartbeat": {HeartbeatSubject(acct, cluster, inst), "ecs.bus.123456789012.default.instance-heartbeat.i-abc123"},
		"assign":    {AssignSubject(acct, cluster, inst), "ecs.bus.123456789012.default.assign.i-abc123"},
		"taskstate": {TaskStateSubject(acct, cluster, task), "ecs.bus.123456789012.default.task-state.t-deadbeef"},
		"reconcile": {ServiceReconcileSubject(acct, cluster), "ecs.bus.123456789012.default.service-reconcile"},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
}
