import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly input: object
  }
  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({})),
    },
  }
})

vi.mock("@/lib/awsClient", () => ({
  getEc2Client: () => ({ send: sdk.send }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    createFileRoute:
      () =>
      <TOptions,>(options: TOptions) => ({
        options,
        useParams: () => ({ id: "eipalloc-1" }),
      }),
    useNavigate: () => vi.fn(),
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { Route } from "./$id"

async function renderPage() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "addresses", "eipalloc-1"], {
    Addresses: [
      {
        AllocationId: "eipalloc-1",
        AssociationId: "eipassoc-1",
        InstanceId: "i-1",
        PublicIp: "198.51.100.7",
      },
    ],
  })
  qc.setQueryData(["ec2", "instances"], { Reservations: [] })
  const Detail = Route.options.component
  if (!Detail) {
    throw new Error("route has no component")
  }
  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, qc)
}

describe("elastic ip detail", () => {
  beforeEach(() => {
    sdk.send.mockClear()
  })

  it("disassociates only after it is confirmed", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("button", { name: "Disassociate" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Disassociate" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { AssociationId: "eipassoc-1" },
    })
  })

  it("does nothing when the dialog is cancelled", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("button", { name: "Disassociate" }))
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(sdk.send).not.toHaveBeenCalled()
  })
})
