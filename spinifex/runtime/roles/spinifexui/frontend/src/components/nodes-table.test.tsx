import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import type { NodeInfo } from "@/queries/admin"
import type { NodeFacts } from "@/queries/nodes"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { NodesTable } from "./nodes-table"

const node = {
  node: "node-1",
  status: "Ready",
  host: "10.0.0.1",
  region: "us-east-1",
  az: "us-east-1a",
  uptime: 300,
  services: ["ec2", "ebs"],
  vm_count: 3,
} as NodeInfo

const facts = {
  node: "node-1",
  form_factor: "rack-server",
  system: { vendor: "Dell Inc.", product: "PowerEdge R7525" },
  os: {},
  cpu: { sockets: 2, cores: 64, threads: 128 },
  memory: { total_bytes: 0, slots_used: 0, slots_total: 0, groups: null },
  nics: [
    { name: "eno1", state: "up", speed_mbps: 1000 },
    { name: "enp193s0f1np1", state: "up", speed_mbps: 25_000 },
    { name: "enp193s0f0np0", state: "down", speed_mbps: 100_000 },
  ],
  planes: null,
  sources: null,
} as NodeFacts

function renderTable(nodes: NodeInfo[], nodeFacts?: NodeFacts[]) {
  const onRowClick = vi.fn()

  render(
    <NodesTable
      facts={nodeFacts}
      nodes={nodes}
      onRowClick={onRowClick}
      onSortChange={vi.fn()}
      sort={{ column: "name", desc: false }}
    />,
  )

  return onRowClick
}

describe("NodesTable", () => {
  it("links the name to the node and colours a ready node green", () => {
    renderTable([node])

    expect(screen.getByRole("link", { name: "node-1" })).toHaveAttribute(
      "href",
      "/nodes",
    )
    expect(screen.getByText("ready")).toHaveAttribute("data-tone", "success")
  })

  it("reads a node that is not ready as a danger state", () => {
    renderTable([{ ...node, status: "NotReady" }])

    expect(screen.getByText("notready")).toHaveAttribute("data-tone", "danger")
  })

  it("shows the model and the fastest port that is up", () => {
    renderTable([node], [facts])

    expect(screen.getByText("Dell Inc. PowerEdge R7525")).toBeInTheDocument()
    expect(screen.getByText("25 GbE")).toBeInTheDocument()
    expect(screen.getByText("128 threads")).toBeInTheDocument()
  })

  it("opens the node when its row is clicked", async () => {
    const onRowClick = renderTable([node])

    await userEvent.setup().click(screen.getByText("10.0.0.1"))

    expect(onRowClick).toHaveBeenCalledWith("node-1")
  })
})
