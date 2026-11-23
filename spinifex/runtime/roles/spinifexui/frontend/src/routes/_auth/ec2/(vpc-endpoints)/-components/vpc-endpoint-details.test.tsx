import type { ServiceDetail, VpcEndpoint } from "@aws-sdk/client-ec2"
import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly constructor: { name: string }
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({})),
    },
  }
})

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import {
  ec2GatewayEndpointServicesQueryOptions,
  ec2RouteTablesQueryOptions,
} from "@/queries/ec2"

import { VpcEndpointDetails } from "./vpc-endpoint-details"

const S3 = "com.amazonaws.ap-southeast-2.s3"

const CUSTOM =
  '{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"*"}]}'

function endpoint(over: Partial<VpcEndpoint> = {}): VpcEndpoint {
  return {
    VpcEndpointId: "vpce-1",
    VpcEndpointType: "Gateway",
    VpcId: "vpc-a",
    ServiceName: S3,
    State: "Available",
    RouteTableIds: ["rtb-a1"],
    ...over,
  }
}

function render(
  vpce: VpcEndpoint,
  services: ServiceDetail[],
  tab: "overview" | "settings" = "overview",
) {
  sdk.send.mockClear()
  sdk.send.mockImplementation(async () => ({}))
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(ec2GatewayEndpointServicesQueryOptions.queryKey, {
    $metadata: {},
    ServiceDetails: services,
  })
  queryClient.setQueryData(ec2RouteTablesQueryOptions.queryKey, {
    $metadata: {},
    RouteTables: [
      { RouteTableId: "rtb-a1", VpcId: "vpc-a" },
      { RouteTableId: "rtb-a2", VpcId: "vpc-a" },
      { RouteTableId: "rtb-b1", VpcId: "vpc-b" },
    ],
  })

  return renderWithClient(
    <VpcEndpointDetails
      endpoint={vpce}
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    queryClient,
  )
}

function sent(name: string) {
  return sdk.send.mock.calls
    .filter(([command]) => command.constructor.name === name)
    .map(([command]) => command.input)
}

const ENFORCED = [{ ServiceName: S3, VpcEndpointPolicySupported: true }]

describe("VpcEndpointDetails", () => {
  it("adds and removes only the route tables that changed", async () => {
    const user = userEvent.setup()
    render(endpoint(), ENFORCED)

    await user.click(
      screen.getByRole("button", { name: "Manage route tables" }),
    )

    const form = screen.getByRole("region", { name: "Manage route tables" })

    expect(
      within(form).queryByRole("switch", { name: "Route table rtb-b1" }),
    ).not.toBeInTheDocument()

    await user.click(
      within(form).getByRole("switch", { name: "Route table rtb-a1" }),
    )
    await user.click(
      within(form).getByRole("switch", { name: "Route table rtb-a2" }),
    )
    await user.click(
      within(form).getByRole("button", { name: "Save route tables" }),
    )

    await waitFor(() => {
      expect(sent("ModifyVpcEndpointCommand")).toStrictEqual([
        {
          VpcEndpointId: "vpce-1",
          AddRouteTableIds: ["rtb-a2"],
          RemoveRouteTableIds: ["rtb-a1"],
          PolicyDocument: undefined,
          ResetPolicy: undefined,
        },
      ])
    })
  })

  it.each([
    ["the service is not offered", []],
    [
      "the service takes no custom policy",
      [{ ServiceName: S3, VpcEndpointPolicySupported: false }],
    ],
  ])("shows the policy but offers no edit when %s", (_, services) => {
    render(endpoint({ PolicyDocument: CUSTOM }), services)

    expect(
      screen.getByText("Custom: only what this document allows."),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: "Edit policy" }),
    ).not.toBeInTheDocument()
  })

  it("resets a custom policy to full access", async () => {
    const user = userEvent.setup()
    render(endpoint({ PolicyDocument: CUSTOM }), ENFORCED)

    await user.click(screen.getByRole("button", { name: "Edit policy" }))

    const form = screen.getByRole("region", { name: "Edit policy" })

    await user.click(within(form).getByRole("radio", { name: "Full access" }))
    await user.click(within(form).getByRole("button", { name: "Save policy" }))

    await waitFor(() => {
      expect(sent("ModifyVpcEndpointCommand")).toStrictEqual([
        {
          VpcEndpointId: "vpce-1",
          AddRouteTableIds: undefined,
          RemoveRouteTableIds: undefined,
          PolicyDocument: undefined,
          ResetPolicy: true,
        },
      ])
    })
  })

  it("cannot save full access over a policy that already is", async () => {
    const user = userEvent.setup()
    render(endpoint(), ENFORCED)

    await user.click(screen.getByRole("button", { name: "Edit policy" }))

    expect(
      within(screen.getByRole("region", { name: "Edit policy" })).getByRole(
        "button",
        { name: "Save policy" },
      ),
    ).toBeDisabled()
  })

  it("keeps the panel's delete open with AWS's reason when the endpoint is refused", async () => {
    const user = userEvent.setup()
    render(endpoint(), ENFORCED, "settings")
    sdk.send.mockImplementation(async (command) =>
      command.constructor.name === "DeleteVpcEndpointsCommand"
        ? {
            Unsuccessful: [
              {
                ResourceId: "vpce-1",
                Error: {
                  Code: "InternalError",
                  Message: "An internal error has occurred.",
                },
              },
            ],
          }
        : {},
    )

    await user.click(screen.getByRole("button", { name: "Delete endpoint" }))

    const dialog = screen.getByRole("alertdialog")

    await user.click(within(dialog).getByRole("button", { name: "Delete" }))

    await expect(
      within(dialog).findByText(/An internal error has occurred\./),
    ).resolves.toBeInTheDocument()
    expect(sent("DeleteVpcEndpointsCommand")).toStrictEqual([
      { VpcEndpointIds: ["vpce-1"] },
    ])
  })

  it("never removes a table that a refresh adds while the form is open", async () => {
    const user = userEvent.setup()
    const { rerender } = render(endpoint(), ENFORCED)

    await user.click(
      screen.getByRole("button", { name: "Manage route tables" }),
    )
    rerender(
      <VpcEndpointDetails
        endpoint={endpoint({ RouteTableIds: ["rtb-a1", "rtb-a2"] })}
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        tab="overview"
      />,
    )

    const form = screen.getByRole("region", { name: "Manage route tables" })

    await user.click(
      within(form).getByRole("switch", { name: "Route table rtb-a1" }),
    )
    await user.click(
      within(form).getByRole("button", { name: "Save route tables" }),
    )

    await waitFor(() => {
      expect(sent("ModifyVpcEndpointCommand")).toStrictEqual([
        expect.objectContaining({
          AddRouteTableIds: undefined,
          RemoveRouteTableIds: ["rtb-a1"],
        }),
      ])
    })
  })

  it("sends a stored policy back as one line, and keeps the draft when it is refused", async () => {
    const user = userEvent.setup()
    render(endpoint({ PolicyDocument: CUSTOM }), ENFORCED)
    sdk.send.mockImplementation(async (command) => {
      if (command.constructor.name === "ModifyVpcEndpointCommand") {
        throw new Error(
          "Custom VPC endpoint policies cannot be set at the moment.",
        )
      }

      return {}
    })

    await user.click(screen.getByRole("button", { name: "Edit policy" }))

    const form = screen.getByRole("region", { name: "Edit policy" })

    await user.click(within(form).getByRole("button", { name: "Save policy" }))

    await expect(
      within(form).findByText(/cannot be set at the moment/),
    ).resolves.toBeInTheDocument()
    expect(sent("ModifyVpcEndpointCommand")).toStrictEqual([
      expect.objectContaining({ PolicyDocument: CUSTOM }),
    ])
    expect(
      JSON.parse(
        within(form).getByRole("textbox", { name: "Policy document" })
          .textContent ?? "",
      ),
    ).toStrictEqual(JSON.parse(CUSTOM))
  })
})
