import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL } from "@/lib/panel-search"
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

  return await mockRouter(importOriginal)
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

function renderPage(targetGroups: TargetGroup[], onSearchChange = vi.fn()) {
  renderWithClient(
    <DescribeTargetGroupsPage
      onSearchChange={onSearchChange}
      search={CLOSED_PANEL}
      targetGroups={targetGroups}
    />,
    createTestQueryClient(),
  )

  return onSearchChange
}

describe("describe-target-groups list route", () => {
  beforeEach(() => {
    sdk.reset()
    sdk.setHandler(
      "DescribeTargetHealthCommand",
      (input: { TargetGroupArn: string }) =>
        input.TargetGroupArn === "arn:tg:1"
          ? {
              TargetHealthDescriptions: [
                { TargetHealth: { State: "healthy" } },
                { TargetHealth: { State: "unhealthy" } },
              ],
            }
          : { TargetHealthDescriptions: [] },
    )
  })

  it("renders target-group rows with health summaries from the mocked SDK", async () => {
    renderPage(TGS)

    expect(screen.getByText("tg-one")).toBeInTheDocument()
    expect(screen.getByText("tg-two")).toBeInTheDocument()
    expect(
      screen.getByRole("link", { name: "Create target group" }),
    ).toHaveAttribute("href", "/ec2/create-target-group")

    await waitFor(() => {
      expect(screen.getByText("1 healthy, 1 unhealthy")).toBeInTheDocument()
    })
  })

  it("selects a target group by its ARN when its row is clicked", async () => {
    const user = userEvent.setup()
    const onSearchChange = renderPage(TGS)

    await user.click(screen.getByText("8080"))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "arn:tg:2",
    })
  })

  it("shows empty state when there are no target groups", () => {
    renderPage([])

    expect(screen.getByText("No target groups yet.")).toBeInTheDocument()
  })
})
