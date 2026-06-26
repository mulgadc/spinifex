import { fireEvent, screen } from "@testing-library/react"
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
vi.mock("./container-instances-tab", () => ({
  ContainerInstancesTab: () => null,
}))

import { ClusterDetailPage } from "./cluster-detail-page"

function seedCluster(extra: Record<string, unknown> = {}) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "clusters", "web"], {
    clusterName: "web",
    clusterArn: "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
    status: "ACTIVE",
    activeServicesCount: 2,
    runningTasksCount: 3,
    pendingTasksCount: 0,
    registeredContainerInstancesCount: 1,
    ...extra,
  })
  return qc
}

describe("ClusterDetailPage", () => {
  it("renders the AWS-aligned tab labels", () => {
    renderWithClient(<ClusterDetailPage clusterName="web" />, seedCluster())

    expect(screen.getByRole("heading", { name: "web" })).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /Delete/ })).toBeInTheDocument()
    for (const label of ["Services", "Tasks", "Infrastructure", "Tags"]) {
      expect(screen.getByRole("tab", { name: label })).toBeInTheDocument()
    }
    expect(
      screen.queryByRole("tab", { name: "Configuration" }),
    ).not.toBeInTheDocument()
  })

  it("shows cluster ARN in the overview panel above the tabs", () => {
    renderWithClient(<ClusterDetailPage clusterName="web" />, seedCluster())

    expect(
      screen.getByText("arn:aws:ecs:ap-southeast-2:123456789012:cluster/web"),
    ).toBeInTheDocument()
  })

  it("renders cluster tags on the Tags tab", () => {
    renderWithClient(
      <ClusterDetailPage clusterName="web" />,
      seedCluster({ tags: [{ key: "env", value: "prod" }] }),
    )

    fireEvent.click(screen.getByRole("tab", { name: "Tags" }))
    expect(screen.getByText("env")).toBeInTheDocument()
    expect(screen.getByText("prod")).toBeInTheDocument()
  })

  it("shows a not-found message when the cluster is missing", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["ecs", "clusters", "web"], null)
    renderWithClient(<ClusterDetailPage clusterName="web" />, qc)

    expect(screen.getByText("Cluster not found.")).toBeInTheDocument()
  })
})
