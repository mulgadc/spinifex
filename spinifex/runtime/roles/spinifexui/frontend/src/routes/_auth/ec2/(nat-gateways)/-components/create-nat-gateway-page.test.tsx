import type { RouteTable, Subnet } from "@aws-sdk/client-ec2"
import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/aws-client", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn(async () => ({})))
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import {
  ec2AddressesQueryOptions,
  ec2RouteTablesQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"

import { CreateNatGatewayPage } from "./create-nat-gateway-page"

// Both subnets assign public IPs on launch; only one routes to a gateway.
const SUBNETS: Subnet[] = [
  {
    SubnetId: "subnet-pub",
    VpcId: "vpc-1",
    CidrBlock: "10.0.1.0/24",
    MapPublicIpOnLaunch: true,
  },
  {
    SubnetId: "subnet-priv",
    VpcId: "vpc-1",
    CidrBlock: "10.0.2.0/24",
    MapPublicIpOnLaunch: true,
  },
]

function setup(routeTables: RouteTable[]) {
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(ec2SubnetsQueryOptions.queryKey, {
    $metadata: {},
    Subnets: SUBNETS,
  })
  queryClient.setQueryData(ec2RouteTablesQueryOptions.queryKey, {
    $metadata: {},
    RouteTables: routeTables,
  })
  queryClient.setQueryData(ec2AddressesQueryOptions.queryKey, {
    $metadata: {},
    Addresses: [],
  })

  renderWithClient(<CreateNatGatewayPage onClose={vi.fn()} />, queryClient)
}

async function subnetOptions(): Promise<(string | null)[]> {
  await userEvent.setup().click(await screen.findByLabelText("Public subnet"))

  const options = await screen.findAllByRole("option")

  return options.map((o) => o.textContent)
}

describe("CreateNatGatewayPage", () => {
  it("offers only subnets whose route table reaches an internet gateway", async () => {
    setup([
      {
        RouteTableId: "rtb-public",
        VpcId: "vpc-1",
        Associations: [{ SubnetId: "subnet-pub" }],
        Routes: [{ DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-1" }],
      },
      {
        RouteTableId: "rtb-main",
        VpcId: "vpc-1",
        Associations: [{ Main: true }],
        Routes: [{ DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" }],
      },
    ])

    await expect(subnetOptions()).resolves.toStrictEqual([
      "subnet-pub (10.0.1.0/24)",
    ])
  })

  it("offers every subnet when none routes to an internet gateway", async () => {
    setup([])

    await expect(subnetOptions()).resolves.toStrictEqual([
      "subnet-pub (10.0.1.0/24)",
      "subnet-priv (10.0.2.0/24)",
    ])
  })
})
