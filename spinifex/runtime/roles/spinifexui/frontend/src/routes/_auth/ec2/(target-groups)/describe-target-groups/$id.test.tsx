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

import { TargetGroupDetailPage } from "../-components/target-group-detail-page"

const TG_ARN = "arn:tg:1"

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["elbv2", "targetGroups", TG_ARN], {
    TargetGroups: [
      {
        TargetGroupArn: TG_ARN,
        TargetGroupName: "my-tg",
        Protocol: "HTTP",
        Port: 80,
        VpcId: "vpc-aaa",
        TargetType: "instance",
        HealthCheckEnabled: true,
        HealthCheckProtocol: "HTTP",
        HealthCheckPath: "/health",
        HealthCheckPort: "traffic-port",
        HealthCheckIntervalSeconds: 30,
        HealthCheckTimeoutSeconds: 5,
        HealthyThresholdCount: 5,
        UnhealthyThresholdCount: 2,
        Matcher: { HttpCode: "200" },
      },
    ],
  })
  qc.setQueryData(["elbv2", "targetGroups", TG_ARN, "attributes"], {
    Attributes: [
      {
        Key: "deregistration_delay.timeout_seconds",
        Value: "300",
      },
    ],
  })
  qc.setQueryData(["elbv2", "tags", TG_ARN], {
    TagDescriptions: [
      {
        ResourceArn: TG_ARN,
        Tags: [{ Key: "env", Value: "prod" }],
      },
    ],
  })
  qc.setQueryData(["ec2", "instances"], { Reservations: [] })

  return qc
}

describe("target-group detail route", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("renders the overview card with TG fields", () => {
    renderWithClient(<TargetGroupDetailPage arn={TG_ARN} />, seed())

    expect(screen.getByText("my-tg")).toBeInTheDocument()
    expect(screen.getByText("Target Group Details")).toBeInTheDocument()
    expect(screen.getByText(TG_ARN)).toBeInTheDocument()
    expect(screen.getByText("instance")).toBeInTheDocument()
  })

  it("renders a not-found state when the target group ARN is unknown", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "targetGroups", TG_ARN], { TargetGroups: [] })
    qc.setQueryData(["elbv2", "targetGroups", TG_ARN, "attributes"], {
      Attributes: [],
    })
    qc.setQueryData(["elbv2", "tags", TG_ARN], { TagDescriptions: [] })

    renderWithClient(<TargetGroupDetailPage arn={TG_ARN} />, qc)

    expect(screen.getByText("Target group not found.")).toBeInTheDocument()
  })
})
