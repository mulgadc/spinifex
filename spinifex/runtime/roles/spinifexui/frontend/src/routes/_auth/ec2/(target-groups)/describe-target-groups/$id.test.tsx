import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

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
    params: { id: encodeURIComponent("arn:tg:1") },
  })
})

import { TargetGroupDetails } from "../-components/target-group-details"
import { Route } from "./$id"

const TG_ARN = "arn:tg:1"
const LB_ARN = "arn:aws:elasticloadbalancing:r:1:loadbalancer/app/my-alb/abc123"

const TG: TargetGroup = {
  TargetGroupArn: TG_ARN,
  TargetGroupName: "my-tg",
  Protocol: "HTTP",
  Port: 80,
  VpcId: "vpc-aaa",
  TargetType: "instance",
  LoadBalancerArns: [LB_ARN],
  HealthCheckEnabled: true,
  HealthCheckPath: "/health",
  Matcher: { HttpCode: "200" },
}

function seed(tgs: TargetGroup[] = [TG]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["elbv2", "targetGroups", TG_ARN], { TargetGroups: tgs })
  qc.setQueryData(["elbv2", "targetGroups", TG_ARN, "attributes"], {
    Attributes: [{ Key: "stickiness.enabled", Value: "true" }],
  })
  qc.setQueryData(["elbv2", "targetGroups", TG_ARN, "health"], {
    TargetHealthDescriptions: [],
  })
  qc.setQueryData(["elbv2", "tags", TG_ARN], { TagDescriptions: [] })
  qc.setQueryData(["ec2", "instances"], { Reservations: [] })

  return qc
}

async function renderRoute(tgs: TargetGroup[]) {
  const Detail = Route.options.component

  if (!Detail) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, seed(tgs))
}

describe("target-group detail route", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("renders the overview with its load balancers and targets", async () => {
    await renderRoute([TG])

    expect(
      screen.getByRole("heading", { name: "my-tg", level: 1 }),
    ).toBeInTheDocument()
    expect(screen.getByText("/health")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "my-alb" })).toHaveAttribute(
      "href",
      `/ec2/describe-load-balancers/${encodeURIComponent(LB_ARN)}`,
    )
    await expect(
      screen.findByText("No targets registered."),
    ).resolves.toBeInTheDocument()
  })

  it("renders a not-found state when the ARN is unknown", async () => {
    await renderRoute([])

    expect(screen.getByText("Target group not found.")).toBeInTheDocument()
  })

  it("deletes from Settings only after the dialog is confirmed", async () => {
    const user = userEvent.setup()
    const remove = vi.fn(() => ({}))
    sdk.setHandler("DeleteTargetGroupCommand", remove)
    renderWithClient(
      <TargetGroupDetails
        layout="page"
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        tab="settings"
        tg={TG}
      />,
      seed(),
    )

    await expect(
      screen.findByRole("switch", { name: "Stickiness" }),
    ).resolves.toBeChecked()
    await user.click(
      screen.getByRole("button", { name: "Delete target group" }),
    )
    expect(remove).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(remove).toHaveBeenCalledWith({ TargetGroupArn: TG_ARN })
    })
  })
})
