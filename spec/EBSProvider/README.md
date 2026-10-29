# EBSProvider specification

The contract that every implementation of the `EBSProvider` interface must meet.

## Terminology

- **Provider:** the service that backs an implementation's operations, such as Viperblock.
  The memory implementation has none.
- **Implementation:** a Go implementation of the `EBSProvider` interface, which adapts between the shapes of the contract and the shapes that its provider expects.
- **Contract:** the behaviour that this specification requires of every implementation, as observed through the interface.

## Open questions

### EBSProvider

- What may a failed call change?
  A failed call leaves the state unchanged, but a timeout can arrive after the provider acted, and a crash can leave work half done.
- Is the schema version part of the contract?
  Every method refuses an unsupported version with `unsupported_version`, and the conformance suite tests it, but the specification does not model versions.
- Are `internal` and `unavailable` the only errors that a failed provider may cause?
- Must a present but malformed ID be refused with `invalid_argument`?
  IDs are opaque here, so only an omitted one is refused.

### Capabilities

#### GetCapabilities

- Is it a lasting promise that capabilities never change, even when an implementation or its provider is upgraded or reconfigured after an earlier answer was cached?

### Volumes

- Who decides whether a volume is `in-use`?
  An implementation may track attachments itself, or derive the state from whether a publication claim is held.

#### CreateVolume

- May the capacity be anywhere in the requested range, as CSI allows?
  An implementation that rounds up would then fail the comparison that tells a retry from a conflicting request.
- May seed data be combined with a source snapshot?
  The Viperblock implementation refuses, the memory implementation accepts.
- Must the source snapshot be completed?
  AWS refuses one that is not.
- Must an implementation store the availability zone, parameters and source snapshot, so that it can detect a conflicting retry?
  The Viperblock implementation does not.
- Should a retry be compared against the original request rather than the current volume?
  A retry after ExpandVolume conflicts on capacity.
- Should a retry with a different source snapshot volume ID conflict?
  It is not compared.
- Is byte equality the meaning of equal parameters, or may an implementation compare them semantically?
- Should `MaxSeedBytes` be modelled?
  Seed data larger than it is refused with `invalid_argument`.

#### GetVolume

- Must an implementation compare the handle?
  The Viperblock implementation ignores it.
- Is `not_found` the right error for a volume that was deleted and recreated under the same ID?

#### PublishVolume

- Should `volume_in_use` depend on the exclusion scope?
  An implementation whose scope is `none` lets two writers in, and one whose scope is `node` cannot see another node, yet both are refused here.
- Must a republish to the same node with a different `read_only` be refused?
  The Viperblock implementation refuses it with `volume_in_use`, while the memory implementation ignores the flag.
- May two volumes on one node share an NBD URI or a device path?
- Must a block export's size equal the volume's capacity?

#### UnpublishVolume

- Must an implementation compare the handle?
  Both the Viperblock and memory implementations ignore it.
- Is a provider that fails to seal the volume a failed provider?
  The Viperblock implementation answers `internal` and leaves the volume published, which the specification allows only when the provider may fail.
- Should a claim that lapses through its TTL be modelled, as a step that happens without a call?

### Snapshots

#### CopySnapshot

- Should a retried copy return the existing copy, as a retried CreateVolume does, rather than `already_exists`?
- May a source that is not completed be copied?
  AWS refuses until it is.
- What state does a copy start in?
  The memory implementation always returns `completed`, while AWS starts a copy as `pending`.

#### DeleteSnapshot

- Is the handle check part of the contract?
  The Viperblock implementation ignores the handle and deletes anyway.
- May a pending snapshot be deleted?
  AWS allows it.
- What does success promise?
  The Go docs say the snapshot is gone, but a snapshot whose handle did not match is left in place.
