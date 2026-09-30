# EC2 instance-command contract, version 1

This package owns the cross-process contract used to direct a command to the
node that owns a running EC2 instance. It is not an in-process service
interface or the owner of instance state.

## Route and purpose

The subject is `ec2.cmd.<instance-id>`. It is request/reply, and the daemon
that hosts that instance's QEMU process is the only subscriber. This route
exists because lifecycle operations such as start, stop, terminate, volume
attach/detach and ENI attach/detach must reach that one process; sending them
through the normal daemon queue group could select a different node.

`InstanceCommandSubject` constructs a targeted subject. The corresponding
subscription and permission pattern is `ec2.cmd.*`: the `*` deliberately
matches exactly one final instance-ID token, not an arbitrary descendant.

## Compatibility boundary

`EC2InstanceCommand`, its nested data objects, their JSON field names and the
subject shape are wire compatibility. Callers and receivers must use this
package rather than rebuild those details. `commands_test.go` preserves the
version-1 JSON shape and subject form.

This package owns shared command payloads and the drain-volume response. It
does not define every action's reply body, authorize the request, store EC2
state or implement NATS transport. Those remain responsibilities of the EC2
domain and its runtime composition.

Any incompatible subject or payload change requires an explicit versioned
contract or an approved compatibility plan; it is not a package refactor.
