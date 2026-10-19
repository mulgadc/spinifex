import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor } from "@testing-library/react"
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

import { DescribeTargetGroupsPage } from "../-components/describe-target-groups-page"

const TGS: TargetGroup[] = [
  {
    TargetGroupArn: "arn:tg:1",
    TargetGroupName: "tg-one",
    Protocol: "HTTP",
    Port: 80,
    VpcId: "vpc-aaa",
    TargetType: "instance",
  },
  {
    TargetGroupArn: "arn:tg:2",
    TargetGroupName: "tg-two",
    Protocol: "HTTP",
    Port: 8080,
    VpcId: "vpc-aaa",
    TargetType: "instance",
  },
]

describe("describe-target-groups list route", () => {
  beforeEach(() => {
    sdk.reset()
  })

  it("renders target-group rows with health summaries from the mocked SDK", async () => {
    sdk.setHandler(
      "DescribeTargetHealthCommand",
      (input: { TargetGroupArn: string }) => {
        const arn = input.TargetGroupArn

        if (arn === "arn:tg:1") {
          return {
            TargetHealthDescriptions: [
              { TargetHealth: { State: "healthy" } },
              { TargetHealth: { State: "unhealthy" } },
            ],
          }
        }

        return { TargetHealthDescriptions: [] }
      },
    )

    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: TGS })

    renderWithClient(<DescribeTargetGroupsPage />, qc)

    expect(screen.getByText("tg-one")).toBeInTheDocument()
    expect(screen.getByText("tg-two")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Target Group" }),
    ).toBeInTheDocument()

    await waitFor(() => {
      expect(screen.getByText("1 healthy, 1 unhealthy")).toBeInTheDocument()
    })
  })

  it("shows empty state when there are no target groups", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [] })

    renderWithClient(<DescribeTargetGroupsPage />, qc)

    expect(screen.getByText("No target groups found.")).toBeInTheDocument()
  })
})
