import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

vi.mock("@/lib/awsClient", () => ({
  getEcsClient: () => ({ send: vi.fn() }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { ClustersListPage } from "./clusters-list-page"

function seed(clusters: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecs", "clusters"], clusters)
  return qc
}

const CLUSTER = {
  clusterName: "web",
  clusterArn: "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
  status: "ACTIVE",
  activeServicesCount: 2,
  runningTasksCount: 3,
  registeredContainerInstancesCount: 1,
}

describe("ClustersListPage", () => {
  it("renders cluster rows", () => {
    renderWithClient(<ClustersListPage />, seed([CLUSTER]))
    expect(screen.getByRole("link", { name: "web" })).toHaveAttribute(
      "href",
      "/ecs/list-clusters/$clusterName",
    )
    expect(screen.getByText("ACTIVE")).toBeInTheDocument()
  })

  it("shows empty state with no clusters", () => {
    renderWithClient(<ClustersListPage />, seed([]))
    expect(screen.getByText("No ECS clusters found.")).toBeInTheDocument()
  })

  it("opens the create dialog", () => {
    renderWithClient(<ClustersListPage />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Create Cluster" }))
    expect(screen.getByText(/A cluster groups/)).toBeInTheDocument()
  })

  it("opens the delete confirmation for a cluster", () => {
    renderWithClient(<ClustersListPage />, seed([CLUSTER]))
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    expect(
      screen.getByText(/permanently deletes cluster "web"/),
    ).toBeInTheDocument()
  })
})
