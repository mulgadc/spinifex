import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
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

  return await mockRouter(importOriginal, {
    navigate: routerState.navigate,
    params: { id: encodeURIComponent("arn:lb:1") },
  })
})

import { LoadBalancerDetails } from "../-components/load-balancer-details"
import { Route } from "./$id"

const LB_ARN = "arn:lb:1"

const ALB: LoadBalancer = {
  LoadBalancerArn: LB_ARN,
  LoadBalancerName: "my-alb",
  DNSName: "my-alb.example",
  Type: "application",
  Scheme: "internet-facing",
  IpAddressType: "ipv4",
  VpcId: "vpc-aaa",
  State: { Code: "active" },
  AvailabilityZones: [{ ZoneName: "az-1", SubnetId: "subnet-a" }],
  SecurityGroups: ["sg-1"],
}

function seed(lbs: LoadBalancer[] = [ALB]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["elbv2", "loadBalancers", LB_ARN], { LoadBalancers: lbs })
  qc.setQueryData(["elbv2", "loadBalancers", LB_ARN, "attributes"], {
    Attributes: [{ Key: "deletion_protection.enabled", Value: "false" }],
  })
  qc.setQueryData(["elbv2", "tags", LB_ARN], {
    TagDescriptions: [
      { ResourceArn: LB_ARN, Tags: [{ Key: "env", Value: "prod" }] },
    ],
  })
  qc.setQueryData(["elbv2", "listeners", LB_ARN], { Listeners: [] })
  qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [] })
  qc.setQueryData(["acm", "certificates"], { CertificateSummaryList: [] })
  qc.setQueryData(["elbv2", "sslPolicies"], { SslPolicies: [] })
  qc.setQueryData(["ec2", "subnets"], {
    Subnets: [
      {
        SubnetId: "subnet-a",
        VpcId: "vpc-aaa",
        CidrBlock: "10.0.1.0/24",
        Tags: [],
      },
    ],
  })
  qc.setQueryData(["ec2", "securityGroups"], {
    SecurityGroups: [
      { GroupId: "sg-1", GroupName: "default", VpcId: "vpc-aaa" },
    ],
  })

  return qc
}

function renderDetails(lb: LoadBalancer, tab: PanelTab) {
  renderWithClient(
    <LoadBalancerDetails
      layout="page"
      lb={lb}
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    seed([lb]),
  )
}

async function renderRoute(lbs: LoadBalancer[]) {
  const Detail = Route.options.component

  if (!Detail) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, seed(lbs))
}

describe("load-balancer detail route", () => {
  beforeEach(() => {
    sdk.reset()
    routerState.navigate.mockReset()
  })

  it("renders the overview for the load balancer", async () => {
    await renderRoute([ALB])

    expect(
      screen.getByRole("heading", { name: "my-alb", level: 1 }),
    ).toBeInTheDocument()
    expect(screen.getByText("internet-facing")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "sg-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-security-groups/sg-1",
    )
    await expect(
      screen.findByRole("link", { name: "subnet-a (10.0.1.0/24)" }),
    ).resolves.toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Listeners" })).toBeNull()
    expect(screen.queryByRole("button", { name: "Attributes" })).toBeNull()
  })

  it("moves to the Listeners tab through the URL", async () => {
    const user = userEvent.setup()
    await renderRoute([ALB])

    await user.click(screen.getByRole("tab", { name: "Listeners" }))

    expect(routerState.navigate).toHaveBeenCalledWith({
      search: { tab: "listeners" },
      replace: true,
    })
  })

  it("lists listeners on their own tab", async () => {
    renderDetails(ALB, "listeners")

    await expect(
      screen.findByText("No listeners configured."),
    ).resolves.toBeInTheDocument()
    expect(screen.queryByText("internet-facing")).toBeNull()
  })

  it("edits attributes on their own tab, not in Settings", async () => {
    renderDetails(ALB, "attributes")

    await expect(
      screen.findByRole("switch", { name: "Deletion protection" }),
    ).resolves.not.toBeChecked()
  })

  it("renders a not-found state when the ARN is unknown", async () => {
    await renderRoute([])

    expect(screen.getByText("Load balancer not found.")).toBeInTheDocument()
  })

  it("edits security groups in Settings for an application load balancer", async () => {
    renderDetails(ALB, "settings")

    await expect(
      screen.findByRole("button", { name: "Security groups" }),
    ).resolves.toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Attributes" })).toBeNull()
  })

  it("has no security groups to edit on a network load balancer", async () => {
    renderDetails(
      { ...ALB, Type: "network", SecurityGroups: undefined },
      "settings",
    )

    await expect(
      screen.findByRole("button", { name: "Subnets" }),
    ).resolves.toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Security groups" })).toBeNull()
  })

  it("deletes only after the dialog is confirmed", async () => {
    const user = userEvent.setup()
    const remove = vi.fn(() => ({}))
    sdk.setHandler("DeleteLoadBalancerCommand", remove)
    renderDetails(ALB, "settings")

    await user.click(
      await screen.findByRole("button", { name: "Delete load balancer" }),
    )
    expect(remove).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(remove).toHaveBeenCalledWith({ LoadBalancerArn: LB_ARN })
    })
  })
})
