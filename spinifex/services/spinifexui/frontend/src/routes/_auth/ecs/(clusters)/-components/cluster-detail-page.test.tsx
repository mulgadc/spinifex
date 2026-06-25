import { screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send: vi.fn() }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => vi.fn(),
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

vi.mock("./services-tab", () => ({ ServicesTab: () => null }))
vi.mock("./tasks-tab", () => ({ TasksTab: () => null }))
vi.mock("./task-definitions-tab", () => ({ TaskDefinitionsTab: () => null }))
vi.mock("./container-instances-tab", () => ({
  ContainerInstancesTab: () => null,
}))

import { ClusterDetailPage } from "./cluster-detail-page"

describe("ClusterDetailPage", () => {
  it("renders the overview with cluster ARN and tab labels", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["ecs", "clusters", "web"], {
      clusterName: "web",
      clusterArn: "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
      status: "ACTIVE",
      activeServicesCount: 2,
      runningTasksCount: 3,
      pendingTasksCount: 0,
      registeredContainerInstancesCount: 1,
    })
    renderWithClient(<ClusterDetailPage clusterName="web" />, qc)

    expect(screen.getByRole("heading", { name: "web" })).toBeInTheDocument()
    expect(
      screen.getByText("arn:aws:ecs:ap-southeast-2:123456789012:cluster/web"),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /Delete/ })).toBeInTheDocument()
    expect(screen.getByRole("tab", { name: "Services" })).toBeInTheDocument()
    expect(
      screen.getByRole("tab", { name: "Container Instances" }),
    ).toBeInTheDocument()
  })
})
