import type { Cluster } from "@aws-sdk/client-eks"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params,
    className,
  }: {
    children: ReactNode
    to: string
    params?: { id?: string }
    className?: string
  }) => (
    <a
      className={className}
      href={params?.id ? to.replace("$id", params.id) : to}
    >
      {children}
    </a>
  ),
}))

import { NetworkingTab } from "./networking-tab"

const CLUSTER: Cluster = {
  endpoint: "https://api.example",
  resourcesVpcConfig: {
    vpcId: "vpc-1",
    subnetIds: ["subnet-a", "subnet-b"],
    securityGroupIds: ["sg-extra"],
    clusterSecurityGroupId: "sg-cluster",
    endpointPublicAccess: true,
    endpointPrivateAccess: false,
  },
}

describe("NetworkingTab", () => {
  it("deep-links the VPC to its EC2 detail route", () => {
    render(<NetworkingTab cluster={CLUSTER} />)
    expect(screen.getByRole("link", { name: "vpc-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-vpcs/vpc-1",
    )
  })

  it("deep-links every subnet", () => {
    render(<NetworkingTab cluster={CLUSTER} />)
    expect(screen.getByRole("link", { name: "subnet-a" })).toHaveAttribute(
      "href",
      "/ec2/describe-subnets/subnet-a",
    )
    expect(screen.getByRole("link", { name: "subnet-b" })).toHaveAttribute(
      "href",
      "/ec2/describe-subnets/subnet-b",
    )
  })

  it("deep-links the cluster security group", () => {
    render(<NetworkingTab cluster={CLUSTER} />)
    expect(screen.getByRole("link", { name: "sg-cluster" })).toHaveAttribute(
      "href",
      "/ec2/describe-security-groups/sg-cluster",
    )
  })

  it("renders endpoint access state", () => {
    render(<NetworkingTab cluster={CLUSTER} />)
    expect(screen.getByText("Enabled")).toBeInTheDocument()
    expect(screen.getByText("Disabled")).toBeInTheDocument()
  })
})
