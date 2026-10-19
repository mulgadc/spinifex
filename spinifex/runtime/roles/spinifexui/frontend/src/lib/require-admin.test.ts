import { isRedirect } from "@tanstack/react-router"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient } from "@/test/render"

const mockStsSend = vi.fn()

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({ send: mockStsSend }),
}))

vi.mock("@aws-sdk/client-sts", () => ({
  // Constructable stub: `new GetCallerIdentityCommand({})` must not throw.
  GetCallerIdentityCommand: vi.fn(),
}))

import { requireAdmin } from "./require-admin"

async function callGuard(queryClient = createTestQueryClient()) {
  await requireAdmin({ context: { queryClient } })
}

describe("requireAdmin", () => {
  beforeEach(() => {
    mockStsSend.mockReset()
  })

  it("lets the admin account through", async () => {
    mockStsSend.mockResolvedValue({ Account: "000000000001" })

    await expect(callGuard()).resolves.toBeUndefined()
  })

  it("redirects any other account home", async () => {
    mockStsSend.mockResolvedValue({ Account: "000000000002" })

    const guard = callGuard()

    await expect(guard).rejects.toSatisfy(isRedirect)
    await expect(guard).rejects.toMatchObject({ options: { to: "/" } })
  })

  it("propagates a failed identity lookup instead of redirecting", async () => {
    const failure = new Error("gateway unreachable")
    mockStsSend.mockRejectedValue(failure)

    await expect(callGuard()).rejects.toBe(failure)
  })

  it("reuses the cached identity across navigations", async () => {
    mockStsSend.mockResolvedValue({ Account: "000000000001" })
    const queryClient = createTestQueryClient()

    await callGuard(queryClient)
    await callGuard(queryClient)

    expect(mockStsSend).toHaveBeenCalledOnce()
  })
})
