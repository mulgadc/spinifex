import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
  getEc2Client: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { CreateDBSubnetGroupPage } from "./create-db-subnet-group-page"

const SUBNETS = [
  {
    SubnetId: "subnet-a1",
    VpcId: "vpc-1",
    CidrBlock: "10.0.1.0/24",
    AvailabilityZone: "ap-southeast-2a",
  },
  {
    SubnetId: "subnet-a2",
    VpcId: "vpc-1",
    CidrBlock: "10.0.2.0/24",
    AvailabilityZone: "ap-southeast-2a",
  },
  {
    SubnetId: "subnet-b1",
    VpcId: "vpc-2",
    CidrBlock: "10.1.1.0/24",
    AvailabilityZone: "ap-southeast-2a",
  },
]

function seed(subnets: unknown[] = SUBNETS) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "subnets"], { Subnets: subnets })
  qc.setQueryData(["ec2", "vpcs"], {
    Vpcs: [
      { VpcId: "vpc-1", Tags: [{ Key: "Name", Value: "prod" }] },
      { VpcId: "vpc-2" },
    ],
  })
  return qc
}

function subnetCheckbox(subnetId: string): HTMLElement {
  return screen.getByRole("checkbox", {
    name: new RegExp(`Subnet ${subnetId}`),
  })
}

describe("CreateDBSubnetGroupPage", () => {
  it("groups the subnets by VPC", () => {
    renderWithClient(<CreateDBSubnetGroupPage />, seed())
    expect(screen.getByText(/vpc-1 \(prod\)/)).toBeInTheDocument()
    expect(subnetCheckbox("subnet-a1")).toBeInTheDocument()
    expect(subnetCheckbox("subnet-b1")).toBeInTheDocument()
  })

  // resolveGroupSubnets refuses a group spanning two VPCs, so the second VPC
  // has to go out of reach rather than stay selectable and fail at submit.
  it("puts the other VPCs out of reach once a subnet is chosen", () => {
    renderWithClient(<CreateDBSubnetGroupPage />, seed())
    expect(subnetCheckbox("subnet-b1")).toBeEnabled()

    fireEvent.click(subnetCheckbox("subnet-a1"))

    expect(subnetCheckbox("subnet-a2")).toBeEnabled()
    expect(subnetCheckbox("subnet-b1")).toBeDisabled()
    expect(screen.getByText(/must span one VPC/)).toBeInTheDocument()
  })

  it("explains an account with no subnets", () => {
    renderWithClient(<CreateDBSubnetGroupPage />, seed([]))
    expect(screen.getByText(/No subnets in this account/)).toBeInTheDocument()
  })

  it("does not offer an AZ-count rule the platform does not enforce", () => {
    renderWithClient(<CreateDBSubnetGroupPage />, seed())
    expect(
      screen.getByText(/the two-AZ rule AWS enforces does not apply here/),
    ).toBeInTheDocument()
  })

  it("sends CreateDBSubnetGroup with the chosen subnets", async () => {
    renderWithClient(<CreateDBSubnetGroupPage />, seed())

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "orders-subnets" },
    })
    fireEvent.change(screen.getByLabelText("Description"), {
      target: { value: "Private subnets" },
    })
    fireEvent.click(subnetCheckbox("subnet-a1"))
    fireEvent.click(subnetCheckbox("subnet-a2"))
    fireEvent.click(screen.getByRole("button", { name: "Create Subnet Group" }))

    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBSubnetGroupName).toBe("orders-subnets")
    expect(input.DBSubnetGroupDescription).toBe("Private subnets")
    expect(input.SubnetIds).toStrictEqual(["subnet-a1", "subnet-a2"])
  })

  it("refuses a reserved name before the round trip", async () => {
    renderWithClient(<CreateDBSubnetGroupPage />, seed())

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "default" },
    })
    fireEvent.change(screen.getByLabelText("Description"), {
      target: { value: "Private subnets" },
    })
    fireEvent.click(subnetCheckbox("subnet-a1"))
    fireEvent.click(screen.getByRole("button", { name: "Create Subnet Group" }))

    expect(await screen.findByText(/the service reserves/)).toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })
})
