import { type Mock, vi } from "vitest"

export interface SdkCommand {
  readonly constructor: { name: string }
  readonly input: unknown
}

// Handlers are keyed by command name, so each one declares the input type of
// its own command; the shared registry cannot name them all.
export type SdkHandler = (input: never) => unknown

export interface FakeSdk {
  readonly send: Mock<(command: SdkCommand) => Promise<unknown>>
  setHandler: (commandName: string, handler: SdkHandler) => void
  // Restores the handlers the fake was created with.
  reset: () => void
}

// A stand-in for SDK clients that answers each command from the handler
// registered under its class name, and throws for a command with none.
export function createFakeSdk(
  initialHandlers: Iterable<readonly [string, SdkHandler]> = [],
): FakeSdk {
  const initial = [...initialHandlers]
  const handlers = new Map<string, SdkHandler>(initial)

  const send = vi.fn(async (command: SdkCommand): Promise<unknown> => {
    const commandName = command.constructor.name
    const handler = handlers.get(commandName)

    if (!handler) {
      throw new Error(`No handler registered for SDK command ${commandName}`)
    }

    return handler(command.input as never)
  })

  return {
    send,
    setHandler: (commandName, handler) => {
      handlers.set(commandName, handler)
    },
    reset: () => {
      handlers.clear()

      for (const [commandName, handler] of initial) {
        handlers.set(commandName, handler)
      }
    },
  }
}
