// Drives the real routeTree, so the router's own param encoding is under test:
// the panel's link encodes the ARN once, and both that URL and an old doubly
// encoded bookmark ask IAM for the raw ARN.
import { QueryClientProvider } from "@tanstack/react-query"
import {
  createMemoryHistory,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

interface SdkCommand {
  readonly constructor: { name: string }
  readonly input: { PolicyArn?: string }
}

const mockSend = vi.hoisted(() =>
  vi.fn<(command: SdkCommand) => Promise<unknown>>(),
)

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@/lib/sts", () => ({
  getStsClient: () => ({
    send: vi.fn().mockResolvedValue({
      Account: "000000000001",
      Arn: "arn:aws:iam::000000000001:user/test",
    }),
  }),
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

import { routeTree } from "../../../../../routeTree.gen"

const POLICY_ARN = "arn:aws:iam::000000000001:policy/AdministratorAccess"

const POLICY = {
  Arn: POLICY_ARN,
  PolicyName: "AdministratorAccess",
  DefaultVersionId: "v1",
}

function policyArnsRequested(): (string | undefined)[] {
  return mockSend.mock.calls
    .filter(([command]) => command.constructor.name === "GetPolicyCommand")
    .map(([command]) => command.input.PolicyArn)
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

  return { router }
}

describe("policy ARN routing", () => {
  beforeEach(() => {
    mockSend.mockReset()
    mockSend.mockImplementation(async (command) => {
      switch (command.constructor.name) {
        case "GetPolicyCommand": {
          return { Policy: POLICY }
        }

        case "ListPoliciesCommand": {
          return { Policies: [POLICY] }
        }

        case "GetPolicyVersionCommand": {
          return { PolicyVersion: { VersionId: "v1", Document: "{}" } }
        }

        default: {
          return {}
        }
      }
    })
  })

  it("opens the policy page from the panel link with the ARN encoded once", async () => {
    const user = userEvent.setup()

    const { router } = renderAppAt(
      `/iam/list-policies?selected=${encodeURIComponent(POLICY_ARN)}`,
    )

    await user.click(
      await screen.findByRole("link", { name: "Open policy page" }),
    )

    await waitFor(() => {
      expect(router.state.location.pathname).toBe(
        `/iam/list-policies/${encodeURIComponent(POLICY_ARN)}`,
      )
    })
    expect(policyArnsRequested()).toContain(POLICY_ARN)
    expect(policyArnsRequested()).not.toContain(encodeURIComponent(POLICY_ARN))
    await expect(
      screen.findByText(POLICY_ARN, { selector: "nav *" }),
    ).resolves.toBeInTheDocument()
  })

  it("still loads a doubly encoded bookmark", async () => {
    renderAppAt(
      `/iam/list-policies/${encodeURIComponent(encodeURIComponent(POLICY_ARN))}`,
    )

    await waitFor(() => {
      expect(policyArnsRequested()).toStrictEqual([POLICY_ARN])
    })
  })
})
