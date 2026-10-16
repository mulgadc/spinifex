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

## Default VPC request

`EnsureDefaultVpcSubject` (`ec2.EnsureDefaultVpc`) is the request/reply route on which the gateway's account creation asks a daemon in the `spinifex-workers` queue group to build the account's default VPC.
Only the subject is owned here today.
The request (`account_id`) and reply (`vpc_id`, `error`) bodies are still declared as anonymous structs on each side; moving them into this package is a contract change that needs its own characterization first.

## Platform inventory

`PlatformInventorySubject` (`ec2.v1.PlatformInventory`) is a request/reply route on which a daemon in the `spinifex-workers` queue group reports one account's key pairs, placement groups, launch templates and their `ec2-tags` entries from the EC2 owner stores.
EC2 owns it: `domains/ec2/accountinventory` produces the reply and the daemon serves it; the only consumer today is `accountteardown`, which binds an adapter but does not call it.

The request names the account in its body (`account_id`, a 12-digit ID) and may carry an opaque `correlation_ref` that is echoed for logs.
The daemon ignores `X-Account-ID` for this subject.
Neither field grants anything: the subject is read-only, writes to no store, and its reply is never authority to delete.

The reply has one `kinds` entry per kind (`key-pair`, `placement-group`, `launch-template`, `tag-index`) with a status and scan start and end times.
`observed-clean-at-scan-time` means every store for that kind was read completely and held nothing at scan time; writers are not fenced, so it is a snapshot and never evidence of absence.
`items` carry only a resource ID, kind and disposition (`live`, `dependency-held` with member instance IDs, or tag-index `residue`), which are the identities the per-kind Describe subjects already return.
Records no existing subject exposes (public-key objects without metadata, launch-template version bodies and name-index entries without a header, and every unreadable record) appear only as aggregate `counts` per kind, disposition and class.
The reply never carries an object key, raw KV key, key or template name, tag key or value, public-key bytes, launch-template data, user data, record body or store error text; `incomplete_reasons` are fixed categories.

### Compatibility boundary

The subject name carries the contract major; `platform_inventory_test.go` preserves the version-1 JSON shape and wire vocabulary.
Producer and consumer are in the coordinated Spinifex release, so the window is N and N-1.
A daemon that predates the subject has no subscription, so the request gets no responder; the consumer treats no responder, a timeout, an error reply, an undecodable reply, a reply for another account, and a missing, repeated or unknown-status kind as `scan-incomplete`, never clean.
An incompatible change to the subject, fields or vocabulary needs a new major.
