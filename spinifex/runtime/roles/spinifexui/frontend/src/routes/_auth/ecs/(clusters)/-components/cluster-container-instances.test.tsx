import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn())
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { ClusterContainerInstances } from "./cluster-container-instances"

function seed(instances: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "clusters", "web", "container-instances"], instances)

  return qc
}

const ACTIVE = {
  containerInstanceArn:
    "arn:aws:ecs:ap-southeast-2:123456789012:container-instance/web/ci-1",
  ec2InstanceId: "i-abc123",
  status: "ACTIVE",
  runningTasksCount: 1,
  pendingTasksCount: 0,
}

describe("ClusterContainerInstances", () => {
  it("renders an ACTIVE instance with drain and deregister actions", () => {
    renderWithClient(
      <ClusterContainerInstances clusterName="web" />,
      seed([ACTIVE]),
    )
    expect(screen.getByRole("link", { name: "i-abc123" })).toHaveAttribute(
      "href",
      "/ec2/describe-instances/i-abc123",
    )
    expect(
      screen.getByRole("button", { name: "Drain container instance" }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Deregister" }),
    ).toBeInTheDocument()
  })

  it("offers Activate for a draining instance", () => {
    renderWithClient(
      <ClusterContainerInstances clusterName="web" />,
      seed([{ ...ACTIVE, status: "DRAINING" }]),
    )
    expect(
      screen.getByRole("button", { name: "Activate container instance" }),
    ).toBeInTheDocument()
  })

  it("opens the drain confirmation", () => {
    renderWithClient(
      <ClusterContainerInstances clusterName="web" />,
      seed([ACTIVE]),
    )
    fireEvent.click(
      screen.getByRole("button", { name: "Drain container instance" }),
    )
    expect(screen.getByText(/Draining force-stops/)).toBeInTheDocument()
  })

  it("shows empty state with no instances", () => {
    renderWithClient(<ClusterContainerInstances clusterName="web" />, seed([]))
    expect(
      screen.getByText("No container instances found."),
    ).toBeInTheDocument()
  })
})
