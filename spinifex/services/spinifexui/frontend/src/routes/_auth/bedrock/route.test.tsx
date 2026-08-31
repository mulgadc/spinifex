import { isNotFound } from "@tanstack/react-router"
import { describe, expect, it, vi } from "vitest"

const mockIsOchreEnabled = vi.fn()
vi.mock("@/lib/cluster-config", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/cluster-config")>()
  return {
    ...actual,
    isOchreEnabled: (): unknown => mockIsOchreEnabled(),
  }
})

import { Route } from "./route"

describe("bedrock route guard", () => {
  it("throws notFound when the Ochre flag is off", () => {
    mockIsOchreEnabled.mockReturnValue(false)

    let caught: unknown
    try {
      // @ts-expect-error -- beforeLoad's real arg shape is not needed here.
      Route.options.beforeLoad?.({})
    } catch (error) {
      caught = error
    }

    expect(isNotFound(caught)).toBeTruthy()
  })

  it("does not throw when the Ochre flag is on", () => {
    mockIsOchreEnabled.mockReturnValue(true)

    expect(() => {
      // @ts-expect-error -- beforeLoad's real arg shape is not needed here.
      Route.options.beforeLoad?.({})
    }).not.toThrow()
  })
})
