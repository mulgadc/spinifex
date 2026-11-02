# Lambda v1 contracts

This package will own the Lambda process contracts declared by ADR-0009:S13.
It holds only the L-GUEST declaration so far, and no wire types: the transport, authentication mechanism and message shapes are not chosen.
It is not an AWS tenant-facing API.

## L-GUEST: node executor to guest agent

**Owner:** the Lambda domain.

**Producers and consumers:** the node-local executor (`runtime/compute/lambda`) and the in-guest Lambda agent (`agents/lambda`), in both directions.

**Operations:** bootstrap with scoped material, readiness, invoke, completion, bounded logs, shutdown.

**Authentication:** mutual, bound to the environment ID and realization generation, using material the user runtime cannot read.
The shared management network is not an implicit trusted path.
The mechanism and transport are open (ADR-0009 M5), and choosing one must not change this authority model.

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
