import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import type { NodeInfo } from "@/queries/admin"

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

describe("NodesTable", () => {
  it("lists the services in full and counts them when compact", () => {
    const { rerender } = render(<NodesTable nodes={[node]} />)
    expect(screen.getByText("ec2,ebs")).toBeInTheDocument()

    rerender(<NodesTable compact={true} nodes={[node]} />)
    expect(screen.getByText("2")).toBeInTheDocument()
    expect(screen.queryByText("ec2,ebs")).not.toBeInTheDocument()
  })

  it("renders a dash when the node holds no role", () => {
    render(<NodesTable nodes={[node]} />)
    expect(screen.getByText("-")).toBeInTheDocument()
  })
})
