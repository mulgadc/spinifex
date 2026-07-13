import type { InstanceTypeInfo } from "@aws-sdk/client-ec2"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { GpuInstanceTypeSelect } from "./gpu-instance-type-select"

const STANDARD_TYPES = [
  { InstanceType: "t3.nano" },
  { InstanceType: "t3.micro" },
] as InstanceTypeInfo[]

const GPU_TYPES = [
  {
    InstanceType: "mig.1g.24gb",
    GpuInfo: {
      Gpus: [
        {
          Name: "RTX Pro 6000 Blackwell SE",
          Manufacturer: "NVIDIA",
          Count: 1,
          MemoryInfo: { SizeInMiB: 24_576 },
        },
      ],
      TotalGpuMemoryInMiB: 24_576,
    },
  },
  {
    InstanceType: "g5.xlarge",
    GpuInfo: {
      Gpus: [
        {
          Name: "A10G",
          Manufacturer: "NVIDIA",
          Count: 1,
          MemoryInfo: { SizeInMiB: 24_576 },
        },
      ],
      TotalGpuMemoryInMiB: 24_576,
    },
  },
] as InstanceTypeInfo[]

describe("GpuInstanceTypeSelect", () => {
  it("groups standard and GPU types under labelled sections", async () => {
    const user = userEvent.setup()
    render(
      <GpuInstanceTypeSelect
        instanceTypes={[...STANDARD_TYPES, ...GPU_TYPES]}
        onValueChange={vi.fn()}
        value=""
      />,
    )

    await user.click(screen.getByRole("combobox"))

    expect(screen.getByText("Standard")).toBeInTheDocument()
    expect(screen.getByText("GPU-Accelerated")).toBeInTheDocument()
    expect(screen.getByRole("option", { name: /t3\.nano/ })).toBeInTheDocument()
    expect(
      screen.getByRole("option", { name: /mig\.1g\.24gb/ }),
    ).toBeInTheDocument()
  })

  it("shows the VRAM annotation on GPU items", async () => {
    const user = userEvent.setup()
    render(
      <GpuInstanceTypeSelect
        instanceTypes={[...STANDARD_TYPES, ...GPU_TYPES]}
        onValueChange={vi.fn()}
        value=""
      />,
    )

    await user.click(screen.getByRole("combobox"))

    expect(
      screen.getByRole("option", { name: /mig\.1g\.24gb — 24 GiB GPU/ }),
    ).toBeInTheDocument()
    expect(screen.getByRole("option", { name: "t3.nano" })).toBeInTheDocument()
  })

  it("shows the availability annotation when provided", async () => {
    const user = userEvent.setup()
    render(
      <GpuInstanceTypeSelect
        availabilityByType={{ "mig.1g.24gb": 4, "t3.nano": 10 }}
        instanceTypes={[...STANDARD_TYPES, ...GPU_TYPES]}
        onValueChange={vi.fn()}
        value=""
      />,
    )

    await user.click(screen.getByRole("combobox"))

    expect(
      screen.getByRole("option", {
        name: /mig\.1g\.24gb — 24 GiB GPU \(4 available\)/,
      }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("option", { name: /t3\.nano \(10 available\)/ }),
    ).toBeInTheDocument()
  })

  it("renders a single ungrouped list when there are no GPU types", async () => {
    const user = userEvent.setup()
    render(
      <GpuInstanceTypeSelect
        instanceTypes={STANDARD_TYPES}
        onValueChange={vi.fn()}
        value=""
      />,
    )

    await user.click(screen.getByRole("combobox"))

    expect(screen.queryByText("Standard")).not.toBeInTheDocument()
    expect(screen.queryByText("GPU-Accelerated")).not.toBeInTheDocument()
    expect(screen.getByRole("option", { name: "t3.nano" })).toBeInTheDocument()
  })

  it("emits the selected value on change", async () => {
    const user = userEvent.setup()
    const onValueChange = vi.fn()
    render(
      <GpuInstanceTypeSelect
        instanceTypes={[...STANDARD_TYPES, ...GPU_TYPES]}
        onValueChange={onValueChange}
        value=""
      />,
    )

    await user.click(screen.getByRole("combobox"))
    await user.click(screen.getByRole("option", { name: /g5\.xlarge/ }))

    expect(onValueChange).toHaveBeenCalledWith("g5.xlarge")
  })

  it("deduplicates repeated instance-type entries", async () => {
    const user = userEvent.setup()
    render(
      <GpuInstanceTypeSelect
        instanceTypes={[...STANDARD_TYPES, STANDARD_TYPES[0]!]}
        onValueChange={vi.fn()}
        value=""
      />,
    )

    await user.click(screen.getByRole("combobox"))

    expect(screen.getAllByRole("option", { name: "t3.nano" })).toHaveLength(1)
  })
})
