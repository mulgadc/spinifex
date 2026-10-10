import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    Link: ({
      children,
      to,
      params,
    }: {
      children: ReactNode
      to?: string
      params?: Record<string, string>
    }) => <a href={`${to}/${params?.id ?? ""}`}>{children}</a>,
  }
})

import type { NodeInfo } from "@/queries/admin"

import { GPUInventoryCard } from "./gpu-inventory-card"

const MIG_NODE: NodeInfo = {
  node: "node-1",
  status: "Ready",
  host: "10.0.0.1",
  region: "ap-southeast-2",
  az: "ap-southeast-2a",
  uptime: 3600,
  services: [],
  vm_count: 3,
  total_vcpu: 32,
  total_mem_gb: 128,
  reserved_vcpu: 0,
  reserved_mem_gb: 0,
  alloc_vcpu: 4,
  alloc_mem_gb: 8,
  total_gpus: 4,
  alloc_gpus: 3,
  instance_types: [],
  gpus: [
    {
      pci_address: "0000:01:00.0",
      model: "RTX Pro 6000 Blackwell SE",
      vram_mib: 98_304,
      mig_enabled: true,
      mig_profile: "1g.24gb",
      slices: [
        {
          gi_id: 0,
          profile: "1g.24gb",
          vram_mib: 24_576,
          mdev_path: "/dev/mdev/0",
          instance_id: "i-abc123",
        },
        {
          gi_id: 1,
          profile: "1g.24gb",
          vram_mib: 24_576,
          mdev_path: "/dev/mdev/1",
        },
      ],
    },
  ],
}

const PASSTHROUGH_NODE: NodeInfo = {
  ...MIG_NODE,
  node: "node-2",
  gpus: [
    {
      pci_address: "0000:02:00.0",
      model: "A10G",
      vram_mib: 24_576,
      mig_enabled: false,
      instance_id: "i-def456",
    },
  ],
}

describe("GPUInventoryCard", () => {
  it("renders one row per physical GPU across nodes", () => {
    render(<GPUInventoryCard nodes={[MIG_NODE, PASSTHROUGH_NODE]} />)

    expect(screen.getByText("node-1")).toBeInTheDocument()
    expect(screen.getByText("node-2")).toBeInTheDocument()
    expect(screen.getByText("RTX Pro 6000 Blackwell SE")).toBeInTheDocument()
    expect(screen.getByText("A10G")).toBeInTheDocument()
    expect(screen.getByText("MIG")).toBeInTheDocument()
    expect(screen.getByText("Passthrough")).toBeInTheDocument()
  })

  it("shows used/total slice counts for MIG and whole-GPU rows", () => {
    render(<GPUInventoryCard nodes={[MIG_NODE, PASSTHROUGH_NODE]} />)

    expect(screen.getByText("1 / 2")).toBeInTheDocument()
    expect(screen.getByText("1 / 1")).toBeInTheDocument()
  })

  it("expands a MIG row to reveal its per-slice tree with a link for claimed slices", async () => {
    const user = userEvent.setup()
    render(<GPUInventoryCard nodes={[MIG_NODE]} />)

    expect(screen.queryByText(/slice 0/)).not.toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: "Expand slices" }))

    expect(screen.getByText(/slice 0/)).toBeInTheDocument()
    expect(screen.getByText(/slice 1/)).toBeInTheDocument()
    const link = screen.getByRole("link", { name: "i-abc123" })
    expect(link).toHaveAttribute("href", "/ec2/describe-instances/$id/i-abc123")
    expect(screen.getByText("free")).toBeInTheDocument()
  })

  it("does not render an expand control for whole-GPU passthrough rows", () => {
    render(<GPUInventoryCard nodes={[PASSTHROUGH_NODE]} />)

    expect(
      screen.queryByRole("button", { name: "Expand slices" }),
    ).not.toBeInTheDocument()
  })
})
