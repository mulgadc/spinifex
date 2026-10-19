// Drives a cold load of an admin-only route through the real routeTree while
// the caller's identity is still being resolved.
import { QueryClientProvider } from "@tanstack/react-query"
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router"
import { render, screen, waitFor } from "@testing-library/react"
import { act } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import type { SignedFetchOptions } from "@/lib/signed-fetch"

const mockStsSend = vi.fn()
const mockAwsSend = vi.hoisted(() => vi.fn())
const mockSignedFetch = vi.fn<(opts: SignedFetchOptions) => Promise<unknown>>()

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({ send: mockStsSend }),
}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockAwsSend)
})

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: async (opts: SignedFetchOptions) => await mockSignedFetch(opts),
  signedAdminFetch: vi.fn(),
}))

vi.mock("@/lib/auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/auth")>()

  return {
    ...actual,
    getCredentials: () => ({
      accessKeyId: "test",
      secretAccessKey: "test",
      sessionToken: "test",
      expiration: "2099-01-01T00:00:00Z",
    }),
  }
})

import { ThemeProvider } from "@/components/theme-provider"
import { SidebarProvider } from "@/components/ui/sidebar"
import { createTestQueryClient } from "@/test/render"

import { routeTree } from "../../../routeTree.gen"

/* oxlint-disable promise/avoid-new -- holding GetCallerIdentity open is the only way to load the route before the identity is known */
function holdIdentity(): (account: string) => void {
  let release!: (identity: { Account: string }) => void

  const identity = new Promise((resolve) => {
    release = resolve
  })

  mockStsSend.mockReturnValue(identity)

  return (account) => {
    release({ Account: account })
  }
}
/* oxlint-enable promise/avoid-new */

function renderAppAt(path: string) {
  const queryClient = createTestQueryClient()

  const router = createRouter({
    routeTree,
    context: { queryClient },
    history: createMemoryHistory({ initialEntries: [path] }),
  })

  render(
    <ThemeProvider defaultTheme="dark" storageKey="spinifex-ui-theme">
      <QueryClientProvider client={queryClient}>
        <SidebarProvider>
          <RouterProvider router={router} />
        </SidebarProvider>
      </QueryClientProvider>
    </ThemeProvider>,
  )

  return { router }
}

describe("admin-only route on a cold load", () => {
  beforeEach(() => {
    mockStsSend.mockReset()
    mockAwsSend.mockReset()
    mockAwsSend.mockResolvedValue({})
    mockSignedFetch.mockReset()
    mockSignedFetch.mockResolvedValue({
      meta_nodes: [],
      blob_nodes: [],
      buckets: [],
    })
  })

  it("renders the page once the identity resolves to the admin account", async () => {
    const resolveIdentity = holdIdentity()
    const { router } = renderAppAt("/s3/service-metrics")

    await waitFor(() => {
      expect(mockStsSend).toHaveBeenCalled()
    })
    await act(async () => {
      resolveIdentity("000000000001")
    })

    await expect(
      screen.findByText("Predastore (S3) Overview"),
    ).resolves.toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/s3/service-metrics")
    expect(screen.queryByText("Something went wrong")).toBeNull()
  })

  it("sends any other account home", async () => {
    const resolveIdentity = holdIdentity()
    const { router } = renderAppAt("/s3/service-metrics")

    await waitFor(() => {
      expect(mockStsSend).toHaveBeenCalled()
    })
    await act(async () => {
      resolveIdentity("000000000002")
    })

    await waitFor(() => {
      expect(router.state.location.pathname).toBe("/")
    })
    expect(screen.queryByText("Predastore (S3) Overview")).toBeNull()
    expect(screen.queryByText("Something went wrong")).toBeNull()
  })
})
