import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen } from "@testing-library/react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { navigate: routerState.navigate })
})

import { DescribeLoadBalancersPage } from "../-components/describe-load-balancers-page"

const LBS: LoadBalancer[] = [
  {
    LoadBalancerArn: "arn:lb:1",
    LoadBalancerName: "lb-one",
    DNSName: "lb-one.example",
    Type: "application",
    Scheme: "internet-facing",
    State: { Code: "active" },
    VpcId: "vpc-aaa",
  },
]

describe("describe-load-balancers list route", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("renders load-balancer rows with resolved fields", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: LBS })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(screen.getByText("lb-one")).toBeInTheDocument()
    expect(screen.getByText("lb-one.example")).toBeInTheDocument()
    expect(screen.getByText("application")).toBeInTheDocument()
    expect(screen.getByText("internet-facing")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Load Balancer" }),
    ).not.toBeDisabled()
  })

  it("shows empty state when no load balancers", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "loadBalancers"], { LoadBalancers: [] })

    renderWithClient(<DescribeLoadBalancersPage />, qc)

    expect(screen.getByText("No load balancers found.")).toBeInTheDocument()
  })
})
