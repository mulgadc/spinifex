import type { Instance } from "@aws-sdk/client-ec2"
import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { ec2InstanceTypesQueryOptions } from "@/queries/ec2"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({})),
    },
  }
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { InstanceOverview } from "./instance-overview"

const INSTANCE: Instance = {
  InstanceId: "i-0a1b2c3d4e5f60001",
  InstanceType: "t3.large",
  State: { Name: "stopped" },
  Tags: [
    { Key: "env", Value: "dev" },
    { Key: "Name", Value: "web-01" },
  ],
}

function renderOverview(instance: Instance = INSTANCE) {
  const queryClient = createTestQueryClient()

  queryClient.setQueryData(ec2InstanceTypesQueryOptions.queryKey, {
    InstanceTypes: [
      {
        InstanceType: "t3.large",
        VCpuInfo: { DefaultVCpus: 2 },
        MemoryInfo: { SizeInMiB: 8192 },
      },
    ],
    $metadata: {},
  })

  renderWithClient(<InstanceOverview instance={instance} />, queryClient)
}

describe("InstanceOverview", () => {
  it("shows the Name tag under the instance ID", () => {
    renderOverview()

    const labels = screen
      .getAllByText(/^(?:Instance ID|Name|Type)$/)
      .map((node) => node.textContent)

    expect(labels.slice(0, 3)).toStrictEqual(["Instance ID", "Name", "Type"])
    expect(screen.getByText("web-01")).toBeInTheDocument()
  })

  it("explains the type's size from DescribeInstanceTypes", async () => {
    const user = userEvent.setup()
    renderOverview()

    await user.hover(screen.getByRole("button", { name: "About Type" }))

    await expect(
      screen.findByText("2 vCPU · 8 GiB memory"),
    ).resolves.toBeInTheDocument()
  })

  it("offers no size for a type the cluster does not list", () => {
    renderOverview({ ...INSTANCE, InstanceType: "m5.24xlarge" })

    expect(screen.getByText("m5.24xlarge")).toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: "About Type" }),
    ).not.toBeInTheDocument()
  })
})
