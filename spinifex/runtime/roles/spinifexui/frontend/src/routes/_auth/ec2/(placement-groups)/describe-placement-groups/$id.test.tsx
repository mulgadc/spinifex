import { screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn(async () => ({})))
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { params: { id: "pg-1" } })
})

import { Route } from "./$id"

async function renderPage() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "placementGroups", "pg-1"], {
    PlacementGroups: [
      {
        GroupId: "pg-1",
        GroupName: "web",
        State: "available",
        Strategy: "spread",
      },
    ],
  })
  qc.setQueryData(["ec2", "instances"], {
    Reservations: [
      {
        Instances: [
          { InstanceId: "i-in", Placement: { GroupName: "web" } },
          { InstanceId: "i-out", Placement: { GroupName: "db" } },
        ],
      },
    ],
  })
  const Detail = Route.options.component

  if (!Detail) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, qc)
}

describe("placement group detail", () => {
  it("lists only the instances placed in this group", async () => {
    await renderPage()

    expect(screen.getByRole("link", { name: "i-in" })).toHaveAttribute(
      "href",
      "/ec2/describe-instances/i-in",
    )
    expect(screen.queryByText("i-out")).not.toBeInTheDocument()
  })
})
