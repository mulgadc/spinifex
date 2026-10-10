import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { renderWithProviders } from "@/test/utils"

const mockUseAdmin = vi.fn()
vi.mock("@/contexts/admin-context", () => ({
  useAdmin: (): unknown => mockUseAdmin(),
}))

vi.mock("@tanstack/react-query", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-query")>()
  return {
    ...actual,
    useQuery: vi.fn(),
  }
})

import { useQuery } from "@tanstack/react-query"

import { AdminDashboardPanel } from "./admin-dashboard-panel"

const mockUseQuery = vi.mocked(useQuery)

const version = {
  version: "1.2.3",
  commit: "abc1234",
  os: "linux",
  arch: "amd64",
  license: "open-source",
}

const nodeData = {
  node: "node-1",
  status: "Ready",
  host: "10.0.0.1",
  region: "us-east-1",
  az: "us-east-1a",
  uptime: 90_061,
  services: ["ec2", "ebs"],
  vm_count: 3,
  total_vcpu: 16,
  total_mem_gb: 32,
  alloc_vcpu: 8,
  alloc_mem_gb: 16,
  instance_types: [],
  nats_role: "leader",
  predastore_role: "primary",
}

function setupMocks({
  isAdmin = true,
  license = "open-source",
  nodes = [nodeData],
  vms = [{ instance_id: "i-1", status: "running" }],
  clusterMode = "single-node",
}: {
  isAdmin?: boolean
  license?: string
  nodes?: (typeof nodeData)[]
  vms?: { instance_id: string; status: string }[]
  clusterMode?: string
} = {}) {
  mockUseAdmin.mockReturnValue({
    isAdmin,
    version: isAdmin ? version : null,
    license,
  })

  mockUseQuery.mockImplementation(((opts: { queryKey: string[] }) => {
    if (opts.queryKey[1] === "nodes") {
      return {
        data: { nodes, cluster_mode: clusterMode },
      } as ReturnType<typeof useQuery>
    }
    return { data: { vms } } as ReturnType<typeof useQuery>
  }) as typeof useQuery)
}

describe("AdminDashboardPanel", () => {
  it("returns null when not admin", () => {
    setupMocks({ isAdmin: false })
    const { container } = renderWithProviders(<AdminDashboardPanel />)
    expect(container.firstChild).toBeNull()
  })

  it("renders version info", () => {
    setupMocks()
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.getByText("1.2.3")).toBeInTheDocument()
    expect(screen.getByText("abc1234")).toBeInTheDocument()
    expect(screen.getByText("linux/amd64")).toBeInTheDocument()
  })

  it("renders region from first node", () => {
    setupMocks()
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.getByText("us-east-1")).toBeInTheDocument()
  })

  it("renders license info for open-source", () => {
    setupMocks({ license: "open-source" })
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.getByText("Open Source (AGPLv3)")).toBeInTheDocument()
    expect(screen.getByText("Community")).toBeInTheDocument()
  })

  it("renders license info for commercial", () => {
    setupMocks({ license: "commercial" })
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.getByText("Commercial")).toBeInTheDocument()
    expect(screen.getByText("Standard")).toBeInTheDocument()
  })

  it("shows upgrade link for open-source license", () => {
    setupMocks({ license: "open-source" })
    renderWithProviders(<AdminDashboardPanel />)
    const links = screen.getAllByRole("link")
    const upgradeLink = links.find((l) =>
      l.textContent?.includes("Upgrade to Commercial"),
    )
    expect(upgradeLink).toBeInTheDocument()
  })

  it("does not show upgrade link for commercial license", () => {
    setupMocks({ license: "commercial" })
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.queryByText(/Upgrade to Commercial/)).not.toBeInTheDocument()
  })

  it("renders cluster status with node and VM counts", () => {
    setupMocks()
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.getByText("single-node")).toBeInTheDocument()
  })

  it("renders vCPU and memory allocation", () => {
    setupMocks()
    renderWithProviders(<AdminDashboardPanel />)
    // vCPU
    expect(screen.getByText("8 / 16")).toBeInTheDocument()
    // memory
    expect(screen.getByText("16.0 / 32.0 GB")).toBeInTheDocument()
  })

  it("renders nodes section as collapsed by default", () => {
    setupMocks()
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.getByText("Nodes (1)")).toBeInTheDocument()
    // The table should not be visible
    expect(screen.queryByText("node-1")).not.toBeInTheDocument()
  })

  it("expands nodes table on click", async () => {
    setupMocks()
    const user = userEvent.setup()
    renderWithProviders(<AdminDashboardPanel />)

    await user.click(screen.getByText("Nodes (1)"))

    expect(screen.getByText("node-1")).toBeInTheDocument()
    expect(screen.getByText("10.0.0.1")).toBeInTheDocument()
    expect(screen.getByText("us-east-1a")).toBeInTheDocument()
    expect(screen.getByText("nats:leader")).toBeInTheDocument()
    expect(screen.getByText("ps:primary")).toBeInTheDocument()
  })

  it("formats uptime in days and hours", async () => {
    setupMocks()
    const user = userEvent.setup()
    renderWithProviders(<AdminDashboardPanel />)
    await user.click(screen.getByText("Nodes (1)"))
    // 90061 seconds = 1d 1h
    expect(screen.getByText("1d 1h")).toBeInTheDocument()
  })

  it("renders add-ons as locked for open-source", () => {
    setupMocks({ license: "open-source" })
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.getByText("Air-gap Support")).toBeInTheDocument()
    expect(screen.getByText("NVIDIA Bluefield DPU")).toBeInTheDocument()
    expect(screen.getByText("NVIDIA MIG Support")).toBeInTheDocument()
  })

  it("renders add-ons as active for commercial", () => {
    setupMocks({ license: "commercial" })
    renderWithProviders(<AdminDashboardPanel />)
    const activeBadges = screen.getAllByText("Active")
    expect(activeBadges).toHaveLength(3)
  })

  it("does not show nodes section when there are no nodes", () => {
    setupMocks({ nodes: [] })
    renderWithProviders(<AdminDashboardPanel />)
    expect(screen.queryByText(/Nodes \(/)).not.toBeInTheDocument()
  })

  it("formats uptime in hours and minutes when less than a day", async () => {
    // 2h 1m
    const shortUptimeNode = { ...nodeData, uptime: 7260 }
    setupMocks({ nodes: [shortUptimeNode] })
    const user = userEvent.setup()
    renderWithProviders(<AdminDashboardPanel />)
    await user.click(screen.getByText("Nodes (1)"))
    expect(screen.getByText("2h 1m")).toBeInTheDocument()
  })

  it("formats uptime in minutes only when less than an hour", async () => {
    // 5m
    const shortUptimeNode = { ...nodeData, uptime: 300 }
    setupMocks({ nodes: [shortUptimeNode] })
    const user = userEvent.setup()
    renderWithProviders(<AdminDashboardPanel />)
    await user.click(screen.getByText("Nodes (1)"))
    expect(screen.getByText("5m")).toBeInTheDocument()
  })
})
