import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import type { VMGPUInfo } from "@/queries/admin"

import { GpuDetailCard } from "./gpu-detail-card"

describe("GpuDetailCard", () => {
  it("renders nothing when no GPUs are attached", () => {
    const { container, rerender } = render(<GpuDetailCard gpus={undefined} />)
    expect(container).toBeEmptyDOMElement()

    rerender(<GpuDetailCard gpus={[]} />)
    expect(container).toBeEmptyDOMElement()
  })

  it("renders MIG slice attachment with profile and mdev path", () => {
    const gpu: VMGPUInfo = {
      model: "RTX Pro 6000 Blackwell SE",
      vram_mib: 24_576,
      profile: "1g.24gb",
      mdev_path: "/sys/bus/mdev/devices/abc-123",
    }
    render(<GpuDetailCard gpus={[gpu]} />)

    expect(screen.getByText("GPU")).toBeInTheDocument()
    expect(screen.getByText("RTX Pro 6000 Blackwell SE")).toBeInTheDocument()
    expect(screen.getByText("24 GiB")).toBeInTheDocument()
    expect(screen.getByText("MIG slice")).toBeInTheDocument()
    expect(screen.getByText("1g.24gb")).toBeInTheDocument()
    expect(
      screen.getByText("/sys/bus/mdev/devices/abc-123"),
    ).toBeInTheDocument()
  })

  it("renders whole-GPU passthrough attachment with PCI address, no profile row", () => {
    const gpu: VMGPUInfo = {
      model: "A10G",
      vram_mib: 24_576,
      pci_address: "0000:01:00.0",
    }
    render(<GpuDetailCard gpus={[gpu]} />)

    expect(screen.getByText("PCIe passthrough")).toBeInTheDocument()
    expect(screen.getByText("0000:01:00.0")).toBeInTheDocument()
    expect(screen.queryByText("Profile")).not.toBeInTheDocument()
  })

  it("renders every attached GPU in attachment order", () => {
    const gpus: VMGPUInfo[] = [
      {
        model: "RTX 3090",
        vram_mib: 24_576,
        pci_address: "0000:01:00.0",
      },
      {
        model: "RTX 3090",
        vram_mib: 24_576,
        pci_address: "0000:02:00.0",
      },
    ]
    render(<GpuDetailCard gpus={gpus} />)

    expect(screen.getByText("GPUs (2)")).toBeInTheDocument()
    expect(screen.getByText("GPU 1")).toBeInTheDocument()
    expect(screen.getByText("GPU 2")).toBeInTheDocument()
    expect(screen.getByText("0000:01:00.0")).toBeInTheDocument()
    expect(screen.getByText("0000:02:00.0")).toBeInTheDocument()
    expect(screen.getAllByText("RTX 3090")).toHaveLength(2)
  })
})
