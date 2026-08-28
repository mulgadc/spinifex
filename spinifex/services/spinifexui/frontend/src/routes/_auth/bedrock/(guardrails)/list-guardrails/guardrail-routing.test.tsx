// Regression test for a route-nesting bug: $guardrailId.tsx used to be the
// PARENT of $guardrailId.edit.tsx (dot-nesting), and since the detail page
// renders no <Outlet/>, navigating to the edit URL matched but never mounted
// the edit form. $guardrailId.tsx was renamed to $guardrailId.index.tsx so
// detail and edit resolve as siblings. This drives real navigation through
// the real routeTree, not just a "route exists" assertion.
import type { GetGuardrailCommandOutput } from "@aws-sdk/client-bedrock"
import { QueryClientProvider } from "@tanstack/react-query"
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router"
import { render, screen, waitFor } from "@testing-library/react"
import { act } from "react"
import { describe, expect, it, vi } from "vitest"

const mockBedrockSend = vi.fn()
const mockStsSend = vi.fn().mockResolvedValue({
  Account: "000000000002",
  Arn: "arn:aws:iam::000000000002:user/test",
})

vi.mock("@/lib/awsClient", () => ({
  getBedrockClient: () => ({ send: mockBedrockSend }),
  getBedrockRuntimeClient: () => ({ send: vi.fn().mockResolvedValue({}) }),
}))

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({ send: mockStsSend }),
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
import { AdminProvider } from "@/contexts/admin-context"
import { createTestQueryClient } from "@/test/elbv2-integration"

import { routeTree } from "../../../../../routeTree.gen"

const GUARDRAIL_ID = "gr-1"

const GUARDRAIL = {
  $metadata: {},
  name: "content-safety",
  description: "Blocks unsafe topics",
  guardrailId: GUARDRAIL_ID,
  guardrailArn: "arn:aws:bedrock:local::guardrail/gr-1",
  version: "DRAFT",
  status: "READY",
  blockedInputMessaging: "Blocked input.",
  blockedOutputsMessaging: "Blocked output.",
  createdAt: new Date("2026-01-01T00:00:00Z"),
  updatedAt: new Date("2026-01-02T00:00:00Z"),
} satisfies GetGuardrailCommandOutput

function seedSend() {
  mockBedrockSend.mockImplementation(
    async (command: { constructor: { name: string } }) => {
      switch (command.constructor.name) {
        case "GetGuardrailCommand": {
          return GUARDRAIL
        }
        case "ListGuardrailsCommand": {
          return { guardrails: [] }
        }
        default: {
          return {}
        }
      }
    },
  )
}

function renderAppAt(path: string) {
  const queryClient = createTestQueryClient()
  const history = createMemoryHistory({ initialEntries: [path] })
  const router = createRouter({
    routeTree,
    context: { queryClient },
    history,
  })
  render(
    <ThemeProvider defaultTheme="dark" storageKey="spinifex-ui-theme">
      <QueryClientProvider client={queryClient}>
        <AdminProvider>
          <SidebarProvider>
            <RouterProvider router={router} />
          </SidebarProvider>
        </AdminProvider>
      </QueryClientProvider>
    </ThemeProvider>,
  )
  return { router }
}

describe("guardrail detail/edit route nesting", () => {
  it("navigating to the edit URL mounts the edit form, not the detail page", async () => {
    seedSend()
    const { router } = renderAppAt(`/bedrock/list-guardrails/${GUARDRAIL_ID}`)

    // Confirm we actually land on the detail leaf first.
    await screen.findByRole("link", { name: /Edit/ })

    await act(async () => {
      await router.navigate({
        to: "/bedrock/list-guardrails/$guardrailId/edit",
        params: { guardrailId: GUARDRAIL_ID },
      })
    })

    await waitFor(() => {
      expect(router.state.location.pathname).toBe(
        `/bedrock/list-guardrails/${GUARDRAIL_ID}/edit`,
      )
    })

    // Regression guard: before the fix, the URL changed but the detail page
    // stayed mounted because the edit route never got an Outlet to render
    // into. The edit form's own controls must actually appear.
    await expect(
      screen.findByRole("button", { name: "Save Changes" }),
    ).resolves.toBeInTheDocument()
    expect(screen.getByLabelText("Name")).toHaveValue("content-safety")
    expect(screen.queryByText("Guardrail tester")).toBeNull()
  })

  it("loading the edit URL directly mounts the edit form", async () => {
    seedSend()
    renderAppAt(`/bedrock/list-guardrails/${GUARDRAIL_ID}/edit`)

    await expect(
      screen.findByRole("button", { name: "Save Changes" }),
    ).resolves.toBeInTheDocument()
  })
})
