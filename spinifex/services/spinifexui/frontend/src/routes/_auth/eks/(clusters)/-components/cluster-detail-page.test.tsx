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
  getEksClient: () => ({ send: vi.fn() }),
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

import { ClusterDetailPage } from "./cluster-detail-page"

const CLUSTER = "demo"

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["eks", "clusters", CLUSTER], {
    cluster: {
      arn: "arn:aws:eks:::cluster/demo",
      status: "ACTIVE",
      version: "1.29",
      platformVersion: "eks.1",
      endpoint: "https://api.demo",
      resourcesVpcConfig: { vpcId: "vpc-1" },
    },
  })
  return qc
}

describe("ClusterDetailPage", () => {
  it("renders overview fields and an active status badge", () => {
    renderWithClient(<ClusterDetailPage clusterName={CLUSTER} />, seed())
    expect(screen.getByText("arn:aws:eks:::cluster/demo")).toBeInTheDocument()
    expect(screen.getByText("https://api.demo")).toBeInTheDocument()
    expect(screen.getAllByText("ACTIVE").length).toBeGreaterThan(0)
    expect(screen.getByRole("tab", { name: "Compute" })).toBeInTheDocument()
  })

  it("falls back to UNKNOWN status when the cluster has none", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["eks", "clusters", CLUSTER], { cluster: {} })
    renderWithClient(<ClusterDetailPage clusterName={CLUSTER} />, qc)
    expect(screen.getByText("UNKNOWN")).toBeInTheDocument()
  })

  it("opens the delete confirmation dialog", () => {
    renderWithClient(<ClusterDetailPage clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: /Delete/ }))
    expect(
      screen.getByText(/permanently deletes cluster "demo"/),
    ).toBeInTheDocument()
  })
})
