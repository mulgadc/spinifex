import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

const clearCredentialsMock = vi.fn()
const clearClientsMock = vi.fn()

const otherTabHandlers: (() => void)[] = []

vi.mock("./auth", () => ({
  clearCredentials: clearCredentialsMock,
  onSessionChangedInOtherTab: (handler: () => void) => {
    otherTabHandlers.push(handler)
    return () => undefined
  },
}))
vi.mock("./awsClient", () => ({ clearClients: clearClientsMock }))

let createQueryClient: typeof import("./query-client").createQueryClient

function awsError(name: string): Error {
  const err = new Error(`${name} occurred`)
  err.name = name
  return err
}

describe("createQueryClient stale-credential recovery", () => {
  beforeEach(async () => {
    // Reset the module so its one-shot `recovering` guard starts fresh per test.
    vi.resetModules()
    vi.stubGlobal("location", { pathname: "/", href: "" })
    ;({ createQueryClient } = await import("./query-client"))
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("clears creds and redirects when a query fails with an auth error", async () => {
    const client = createQueryClient()
    const err = awsError("InvalidClientTokenId")

    await expect(
      client.query({
        queryKey: ["probe"],
        queryFn: async () => {
          throw err
        },
        retry: false,
      }),
    ).rejects.toBe(err)

    expect(clearCredentialsMock).toHaveBeenCalledOnce()
    expect(clearClientsMock).toHaveBeenCalledOnce()
    expect(window.location.href).toBe("/login?reason=expired")
  })

  it("leaves authorization denials to the normal error UI", async () => {
    const client = createQueryClient()
    const err = awsError("AccessDenied")

    await expect(
      client.query({
        queryKey: ["probe"],
        queryFn: async () => {
          throw err
        },
        retry: false,
      }),
    ).rejects.toBe(err)

    expect(clearCredentialsMock).not.toHaveBeenCalled()
    expect(window.location.href).toBe("")
  })

  it("recovers from a failed mutation with an auth error", async () => {
    const client = createQueryClient()
    const err = awsError("ExpiredToken")

    await expect(
      client
        .getMutationCache()
        .build(client, {
          mutationFn: async () => {
            throw err
          },
          retry: false,
        })
        .execute(undefined),
    ).rejects.toBe(err)

    expect(clearCredentialsMock).toHaveBeenCalledOnce()
    expect(window.location.href).toBe("/login?reason=expired")
  })
})

describe("followSessionChangesInOtherTabs", () => {
  beforeEach(() => {
    otherTabHandlers.length = 0
    vi.stubGlobal("location", { pathname: "/ec2", href: "" })
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("drops clients and cached data and goes to /login", async () => {
    const { createQueryClient: create, followSessionChangesInOtherTabs } =
      await import("./query-client")
    const client = create()
    client.setQueryData(["ec2", "instances"], { Reservations: [] })
    followSessionChangesInOtherTabs(client)

    otherTabHandlers[0]?.()

    expect(clearClientsMock).toHaveBeenCalledOnce()
    expect(client.getQueryData(["ec2", "instances"])).toBeUndefined()
    expect(window.location.href).toBe("/login")
  })
})
