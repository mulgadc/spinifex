// Drives a load of the Branding route through the real routeTree, as the
// operator and as a tenant.
import { QueryClientProvider } from "@tanstack/react-query"
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router"
import { render, screen, waitFor } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

const mockStsSend = vi.fn()
const mockAwsSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({ send: mockStsSend }),
}))

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockAwsSend)
})

vi.mock("@/lib/signed-fetch", () => ({
  signedFetch: vi.fn(async () => await Promise.resolve({})),
  signedAdminFetch: vi.fn(async () => await Promise.resolve({ revision: 0 })),
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
import { loadBranding, resetBrandingForTest } from "@/lib/branding"
import { createTestQueryClient } from "@/test/render"

import { routeTree } from "../../../routeTree.gen"

const ADMIN = "000000000001"
const TENANT = "000000000002"

async function loadBuiltInBrand() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => await Promise.resolve(Response.json({}))),
  )
  await loadBranding()
}

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

  return router
}

describe("Settings → Branding route", () => {
  beforeEach(() => {
    mockStsSend.mockReset()
    mockAwsSend.mockReset()
    mockAwsSend.mockResolvedValue({})
  })

  afterEach(() => {
    resetBrandingForTest()
    vi.unstubAllGlobals()
  })

  it("opens for the operator", async () => {
    await loadBuiltInBrand()
    mockStsSend.mockResolvedValue({ Account: ADMIN })
    const router = renderAppAt("/settings/branding")

    await expect(
      screen.findByLabelText("Product name"),
    ).resolves.toBeInTheDocument()
    expect(router.state.location.pathname).toBe("/settings/branding")
  })

  it("sends a tenant account home", async () => {
    await loadBuiltInBrand()
    mockStsSend.mockResolvedValue({ Account: TENANT })
    const router = renderAppAt("/settings/branding")

    await waitFor(() => {
      expect(router.state.location.pathname).toBe("/")
    })
    expect(screen.queryByLabelText("Product name")).toBeNull()
  })
})
