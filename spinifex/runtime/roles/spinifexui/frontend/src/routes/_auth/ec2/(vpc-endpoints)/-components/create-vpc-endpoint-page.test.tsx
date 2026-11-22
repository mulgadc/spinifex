import type { ServiceDetail } from "@aws-sdk/client-ec2"
import { screen, waitFor } from "@testing-library/react"
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
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({
        VpcEndpoint: { VpcEndpointId: "vpce-new" },
      })),
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
  ec2VpcsQueryOptions,
} from "@/queries/ec2"

import { CreateVpcEndpointPage } from "./create-vpc-endpoint-page"

const S3 = "com.amazonaws.ap-southeast-2.s3"

function setup(services: ServiceDetail[]) {
  sdk.send.mockClear()
  const onClose = vi.fn()
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(ec2VpcsQueryOptions.queryKey, {
    $metadata: {},
    Vpcs: [
      { VpcId: "vpc-a", CidrBlock: "10.0.0.0/16" },
      { VpcId: "vpc-b", CidrBlock: "10.1.0.0/16" },
    ],
  })
  queryClient.setQueryData(ec2GatewayEndpointServicesQueryOptions.queryKey, {
    $metadata: {},
    ServiceDetails: services,
  })
  queryClient.setQueryData(ec2RouteTablesQueryOptions.queryKey, {
    $metadata: {},
    RouteTables: [
      {
        RouteTableId: "rtb-a1",
        VpcId: "vpc-a",
        Associations: [{ Main: true }],
      },
      { RouteTableId: "rtb-a2", VpcId: "vpc-a" },
      { RouteTableId: "rtb-b1", VpcId: "vpc-b" },
    ],
  })
  renderWithClient(<CreateVpcEndpointPage onClose={onClose} />, queryClient)

  return onClose
}

function created() {
  return sdk.send.mock.calls
    .filter(
      ([command]) => command.constructor.name === "CreateVpcEndpointCommand",
    )
    .map(([command]) => command.input)
}

async function pickVpc(option: string) {
  const user = userEvent.setup()

  await user.click(screen.getByRole("combobox", { name: "VPC" }))
  await user.click(await screen.findByRole("option", { name: option }))
}

const OFFERED = [{ ServiceName: S3, VpcEndpointPolicySupported: true }]

describe("CreateVpcEndpointPage", () => {
  it("creates an S3 gateway endpoint on the chosen VPC's route tables", async () => {
    const user = userEvent.setup()
    const onClose = setup(OFFERED)

    await pickVpc("vpc-a (10.0.0.0/16)")

    expect(
      screen.queryByRole("switch", { name: "Route table rtb-b1" }),
    ).not.toBeInTheDocument()

    await user.click(screen.getByRole("switch", { name: "Route table rtb-a2" }))
    await user.click(screen.getByRole("button", { name: "Create endpoint" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("vpce-new")
    })
    expect(created()).toStrictEqual([
      {
        VpcEndpointType: "Gateway",
        VpcId: "vpc-a",
        ServiceName: S3,
        RouteTableIds: ["rtb-a2"],
        PolicyDocument: undefined,
        TagSpecifications: undefined,
      },
    ])
  })

  it("clears the chosen route tables when the VPC changes", async () => {
    const user = userEvent.setup()
    setup(OFFERED)

    await pickVpc("vpc-a (10.0.0.0/16)")
    await user.click(screen.getByRole("switch", { name: "Route table rtb-a1" }))
    await pickVpc("vpc-b (10.1.0.0/16)")

    expect(
      screen.getByRole("switch", { name: "Route table rtb-b1" }),
    ).not.toBeChecked()

    await user.click(screen.getByRole("button", { name: "Create endpoint" }))

    await waitFor(() => {
      expect(created()).toHaveLength(1)
    })
    expect(created()[0]).toMatchObject({
      VpcId: "vpc-b",
      RouteTableIds: undefined,
    })
  })

  it("sends a custom policy, and refuses one that is not JSON", async () => {
    const user = userEvent.setup()
    const onClose = setup(OFFERED)

    await pickVpc("vpc-a (10.0.0.0/16)")
    await user.click(screen.getByRole("radio", { name: "Custom" }))

    const editor = screen.getByRole("textbox", { name: "Policy document" })

    await user.clear(editor)
    await user.click(editor)
    await user.paste("{")
    await user.click(screen.getByRole("button", { name: "Create endpoint" }))

    await expect(
      screen.findByText("Policy must be valid JSON"),
    ).resolves.toBeInTheDocument()
    expect(created()).toStrictEqual([])

    const policy =
      '{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"*"}]}'

    await user.clear(editor)
    await user.click(editor)
    await user.paste(policy)
    await user.click(screen.getByRole("button", { name: "Create endpoint" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("vpce-new")
    })
    // The editor lays the document out on blur; the policy itself is unchanged.
    expect(created()).toStrictEqual([
      expect.objectContaining({
        PolicyDocument: JSON.stringify(JSON.parse(policy), null, 2),
      }),
    ])
  })

  it("offers no policy choice where the service does not take one", async () => {
    setup([{ ServiceName: S3, VpcEndpointPolicySupported: false }])

    expect(
      screen.queryByRole("radio", { name: "Custom" }),
    ).not.toBeInTheDocument()
  })

  it("says no service is offered and cannot be sent", () => {
    setup([])

    expect(
      screen.getByText(
        "This formation does not offer a gateway endpoint service yet.",
      ),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create endpoint" }),
    ).toBeDisabled()
  })
})
