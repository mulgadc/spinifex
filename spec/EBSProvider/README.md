# EBSProvider specification

The contract that every implementation of the `EBSProvider` interface must meet.

## Terminology

- **Provider:** the service that backs an implementation's methods, such as Viperblock.
  The memory implementation has none.
- **Implementation:** a Go implementation of the `EBSProvider` interface, which adapts between the shapes of the contract and the shapes that its provider expects.
- **Contract:** the behaviour that this specification requires of every implementation, as observed through the interface.

## Open questions

### EBSProvider

- What may a failed call change?
  A failed call leaves the state unchanged, but a timeout can arrive after the provider acted, and a crash can leave work half done.
- Is the schema version part of the contract?
  Every method refuses an unsupported version with `unsupportedVersion`, and the conformance suite tests it, but the specification does not model versions.
  A block export arrives at contract version 2: a PublishVolume call below it, for a volume that only a host block device can export, is refused with `unsupportedVersion`.
  The `snapshotId` filter on ListSnapshots arrives at contract version 4: a filtered call below it is refused with `unsupportedVersion` rather than answered with every snapshot.
- Are `internal` and `unavailable` the only errors that a failed provider may cause?
- Must a present but malformed ID be refused with `invalidArgument`?
  IDs are opaque here, so only an omitted one is refused.

### Volumes

#### ListVolumes

- Must following next tokens list every volume that exists throughout the walk exactly once?
  The conformance suite tests it, but the specification describes each call alone, so it does not say how one page relates to the next.
- What should a starting token that the implementation never issued do?
  Every implementation treats any token as a position to resume after, while CSI refuses an invalid one with `ABORTED`.
- May a page hold fewer refs than requested yet carry a next token?
  AWS and CSI allow it, but here a next token comes only with a full page, so that a walk cannot stall on empty pages.

#### PublishVolume

- Should `volumeInUse` depend on the exclusion scope?
  An implementation whose scope is `none` lets two writers in, and one whose scope is `node` cannot see another node, yet both are refused here.

#### UnpublishVolume

- Is a provider that fails to seal the volume a failed provider?
  The Viperblock implementation answers `internal` and leaves the volume published, which the specification allows only when the provider may fail.
- Should a claim that lapses through its TTL be modelled, as a step that happens without a call?
- Should a volume that does not exist be refused with `notFound`?
  It succeeds here, as CSI requires and the conformance suite tests, while AWS refuses `DetachVolume` on a missing volume with `InvalidVolume.NotFound`.

#### DeleteVolume

- Should a volume that does not exist be refused with `notFound`?
  It succeeds here, as CSI requires and the conformance suite tests, while AWS refuses `DeleteVolume` on a missing volume with `InvalidVolume.NotFound`.

### Snapshots

#### CreateSnapshot

- May a provider forget a snapshot's source?
  The control plane mints every snapshot ID and records it before the first call, so the ID alone names the request. A provider that no longer knows a snapshot's source, such as qemu-nbd after a restart, answers a retry with the snapshot, and refuses with `alreadyExists` only when it knows the source differs. The specification models a provider that always knows the source.
- Should a copy's ID count as a retry?
  A copy keeps its source's volume ID, so creating a snapshot of that volume under the copy's ID returns the copy.
- Should `crashConsistentSnapshot` be modelled?
  The specification does not model volume data, so it cannot say what a snapshot of a published volume holds.

#### CopySnapshot

- Should a retried copy return the existing copy, as a retried CreateVolume does, rather than `alreadyExists`?

#### ListSnapshots

- The questions for ListVolumes apply here too.
- Must a ref carry the source volume ID?
  The Viperblock implementation leaves it empty, since reading it costs a fetch per snapshot, yet reconciliation needs it to act on a snapshot whose metadata is lost.

#### DeleteSnapshot

- Should a snapshot that does not exist be refused with `notFound`?
  It succeeds here, as CSI requires and the conformance suite tests, while AWS refuses `DeleteSnapshot` on a missing snapshot with `InvalidSnapshot.NotFound`.

## TODO

- Model the data plane.
- Write action header comments.
