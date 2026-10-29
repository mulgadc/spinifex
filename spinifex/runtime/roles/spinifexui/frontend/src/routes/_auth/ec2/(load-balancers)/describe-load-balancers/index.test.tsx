import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
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

function renderPage(loadBalancers: LoadBalancer[], onSearchChange = vi.fn()) {
  renderWithClient(
    <DescribeLoadBalancersPage
      loadBalancers={loadBalancers}
      onSearchChange={onSearchChange}
      search={CLOSED_PANEL}
    />,
    createTestQueryClient(),
  )

  return onSearchChange
}

describe("describe-load-balancers list route", () => {
  it("renders load-balancer rows with resolved fields", () => {
    renderPage(LBS)

    expect(screen.getByText("lb-one")).toBeInTheDocument()
    expect(screen.getByText("lb-one.example")).toBeInTheDocument()
    expect(screen.getByText("application")).toBeInTheDocument()
    expect(screen.getByText("internet-facing")).toBeInTheDocument()
    expect(
      screen.getByRole("link", { name: "Create load balancer" }),
    ).toHaveAttribute("href", "/ec2/create-load-balancer")
  })

  it("selects a load balancer by its ARN when its row is clicked", async () => {
    const user = userEvent.setup()
    const onSearchChange = renderPage(LBS)

    await user.click(screen.getByText("internet-facing"))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "arn:lb:1",
    })
  })

  it("shows empty state when no load balancers", () => {
    renderPage([])

    expect(screen.getByText("No load balancers yet.")).toBeInTheDocument()
  })
})
