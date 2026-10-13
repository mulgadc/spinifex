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
        useParams: () => ({ id: "rtb-1" }),
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
  qc.setQueryData(["ec2", "routeTables", "rtb-1"], {
    RouteTables: [
      {
        RouteTableId: "rtb-1",
        VpcId: "vpc-1",
        Routes: [
          { DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" },
          { DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1" },
        ],
        Associations: [
          { RouteTableAssociationId: "rtbassoc-1", SubnetId: "subnet-1" },
        ],
      },
    ],
  })
  qc.setQueryData(["ec2", "internetGateways"], { InternetGateways: [] })
  qc.setQueryData(["ec2", "natGateways"], { NatGateways: [] })
  qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
  const Detail = Route.options.component
  if (!Detail) {
    throw new Error("route has no component")
  }
  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, qc)
}

describe("route table detail", () => {
  beforeEach(() => {
    sdk.send.mockClear()
  })

  it("deletes a route only after it is confirmed", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole("button", { name: "Delete route to 0.0.0.0/0" }),
    )
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { RouteTableId: "rtb-1", DestinationCidrBlock: "0.0.0.0/0" },
    })
  })

  it("keeps the route when the dialog is cancelled", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole("button", { name: "Delete route to 0.0.0.0/0" }),
    )
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("disassociates a subnet only after it is confirmed", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole("button", { name: "Disassociate subnet-1" }),
    )
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Disassociate" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { AssociationId: "rtbassoc-1" },
    })
  })
})
