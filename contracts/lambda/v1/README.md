# Lambda v1 contracts

This package will own the Lambda process contracts declared by ADR-0009:S13.
It holds the L-GUEST declaration and, in `guest.go`, the connection vocabulary both ends share: the scope, the URI SAN roles, the ALPN identifier and the fw_cfg entry names. The transport and authentication are decided (ADR-0009:S17); the invocation message types and framing are not chosen, so no message types are defined here.
It is not an AWS tenant-facing API.

## L-GUEST: node executor to guest agent

**Owner:** the Lambda domain.

**Producers and consumers:** the node-local executor (`runtime/compute/lambda`) and the in-guest Lambda agent (`agents/lambda`), in both directions.

**Operations:** bootstrap with scoped material, readiness, invoke, completion, bounded logs, shutdown.

**Transport:** TCP over the microVM's network (ADR-0009:S17).
The agent dials one connection to a listener the executor owns for the environment, and the guest exposes no listener for host control.
At most one authenticated control connection is active per environment, and the agent may reconnect; how a prior connection and its in-flight work are handled is open (ADR-0009 M1).
The network is shared and untrusted: no source address, port or bridge membership is identity or authority.
Tenant code may be able to reach the listener, but it cannot open the agent's connection or key material, so any connection it makes fails authentication.

**Authentication:** mutual TLS 1.3, bound to the environment ID and realization generation, using material the user runtime cannot read (ADR-0009:S17).
For each generation the executor creates ephemeral self-signed host and guest certificates whose single URI SAN is `spinifex-lambda://<Region>/<account>/<function ID>/<environment ID>/<generation>/<role>`, with role `host` or `guest`.
Each end validates the peer certificate against the one pinned peer certificate and separately requires that URI SAN with the opposite role; hostname verification is not relied on.
ALPN carries the L-GUEST major version, `spinifex-lambda-guest/1`, and an unsupported version fails the handshake. Builds with the `lambdatestlink` tag offer only `spinifex-lambda-testlink/2` on both host and guest, whose first frame carries the execution-role credential, so a test-link end and a release end fail the handshake; release builds are unchanged.
In the same builds, the gateway reaches the host daemon on the local abstract socket `spinifex-lambda-testlink-control/3`, and each request carries the runtime-image digest the gateway was built with; the daemon refuses a missing or different digest before starting the attempt, and a gateway and daemon on different socket names never connect.
On that socket a request's raw payload is at most 6 MiB, checked by the gateway before encoding and by the daemon after decoding.
Its encoded frame is at most the base64 length of 6 MiB plus 64 KiB of metadata headroom and 64 bytes of field slack, and a receiver checks the frame length before allocating; metadata has no separate bound within the frame.
The credential travels as its own frame of at most 8 KiB, and the reply has the same payload and frame bounds as the request.
The daemon holds at most two connections at once, from accept to close, and leaves the rest unaccepted in the listen backlog.
Under TLS 1.3 the guest's handshake can complete before the host refuses its certificate, so the guest treats the connection as accepted only after the host's first message.
In the same builds, the function owner moves a created function with stored code from `Pending` to `Active` once this node's daemon serves the pinned image and the cluster configuration has exactly one node; a function with environment variables or a code copy over the test-link bound stays `Pending`, and `StateReason` names the unmet condition.
Release builds never leave `Pending`.
The Mulga deploy role installs a test-link build with `deploy_lambda_testlink=true`; this profile does not meet Q-199 fail-closed revocation, and its guests take management addresses whose record can expire (mulga-0hsub).

**Bootstrap material:** the executor passes these fw_cfg entries under `opt/spinifex/lambda/`, and the agent reads them as root and unloads `qemu_fw_cfg` before user code starts.

| Entry | Content |
|---|---|
| `guest-key` | Guest private key, PEM |
| `guest-cert` | Guest certificate, PEM |
| `host-cert` | The one pinned host certificate, PEM |
| `expect` | `<Region>/<account>/<function ID>/<environment ID>/<generation>` |
| `host-addr` | Host listener as `host:port` |

The guest's own interface settings come from `opt/spinifex/netcfg`, the entry other system VMs use.
The agent unloads `qemu_fw_cfg` even when an entry is missing, and refuses to start if the fw_cfg tree is still readable afterwards.
Until the message format exists, the agent authenticates and holds the connection but accepts no host message and starts no user code.

**Authority:** one environment's attempt only.
The user runtime never holds these keys, and sharing the guest with the agent gives user code no platform authority.

**Idempotency and fencing:** an invoke carries the attempt-grant identity and the attempt ID.
The agent accepts a run command only under a current grant, and a completion under a superseded grant is discarded and reported.
How a grant is enforced and proven ended is open (ADR-0009 M1).

**Errors and versions:** the agent and executor fail closed on an unknown protocol version or an identity mismatch.

**Readiness:** environment readiness needs authenticated, current-generation agent evidence; a running hypervisor process does not count.

**Payloads:** the contract must carry a synchronous payload and response of up to 6 MB and an asynchronous payload of up to 1 MB, plus framing, over a path whose size bound is declared.
How payloads move is open (ADR-0009 M3).

### Compatibility boundary

The agent ships inside the runtime image pinned by each published version, so it is outside the coordinated release train.
The executor supports every L-GUEST major carried by a still-supported runtime image.
Retiring an image follows a published runtime-deprecation lifecycle that blocks create and update before it blocks invoke.

Evidence required before the first agent image ships: a function on an old image invoked by a new executor, and the reverse during rollback.

## L-RUNTIME

The guest agent serves the AWS Lambda Runtime API, version `2018-06-01`, to the user runtime at `AWS_LAMBDA_RUNTIME_API`.
Spinifex does not version this AWS contract, and conformance is measured against AWS.
