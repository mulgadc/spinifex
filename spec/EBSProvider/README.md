# EBSProvider specification

The contract that every implementation of the `EBSProvider` interface must meet.

## Terminology

- **Provider:** the service that backs an implementation's operations, such as viperblock.
  The memory implementation has none.
- **Implementation:** a Go implementation of the `EBSProvider` interface, which adapts between the shapes of the contract and the shapes that its provider expects.
- **Contract:** the behaviour that this specification requires of every implementation, as observed through the interface.
