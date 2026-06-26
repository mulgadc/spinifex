import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send: vi.fn() }),
}))

import { ContainerInstancesTab } from "./container-instances-tab"

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

describe("ContainerInstancesTab", () => {
  it("renders an ACTIVE instance with drain and deregister actions", () => {
    renderWithClient(
      <ContainerInstancesTab clusterName="web" />,
      seed([ACTIVE]),
    )
    expect(screen.getByText("i-abc123")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Drain container instance" }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Deregister" }),
    ).toBeInTheDocument()
  })

  it("offers Activate for a draining instance", () => {
    renderWithClient(
      <ContainerInstancesTab clusterName="web" />,
      seed([{ ...ACTIVE, status: "DRAINING" }]),
    )
    expect(
      screen.getByRole("button", { name: "Activate container instance" }),
    ).toBeInTheDocument()
  })

  it("opens the drain confirmation", () => {
    renderWithClient(
      <ContainerInstancesTab clusterName="web" />,
      seed([ACTIVE]),
    )
    fireEvent.click(
      screen.getByRole("button", { name: "Drain container instance" }),
    )
    expect(screen.getByText(/Draining force-stops/)).toBeInTheDocument()
  })

  it("shows empty state with no instances", () => {
    renderWithClient(<ContainerInstancesTab clusterName="web" />, seed([]))
    expect(
      screen.getByText("No container instances found."),
    ).toBeInTheDocument()
  })
})
