import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({
        Subnet: { SubnetId: "subnet-new" },
      })),
    },
  }
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { params: {} })
})

import { Route } from "./create-subnet"

async function renderPage() {
  sdk.send.mockClear()
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "vpcs"], {
    Vpcs: [{ VpcId: "vpc-1", CidrBlock: "10.0.0.0/16" }],
  })
  qc.setQueryData(["ec2", "availabilityZones"], { AvailabilityZones: [] })
  const Page = Route.options.component

  if (!Page) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Page.preload?.()
  renderWithClient(<Page />, qc)
}

describe("create subnet", () => {
  it("creates the subnet without touching its attributes by default", async () => {
    const user = userEvent.setup()
    await renderPage()

    expect(
      screen.getByRole("switch", {
        name: "Enable auto-assign public IPv4 address",
      }),
    ).not.toBeChecked()
    await user.click(screen.getByRole("button", { name: "Create Subnet" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledOnce()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { VpcId: "vpc-1", CidrBlock: "10.0.1.0/24" },
    })
  })

  it("turns on auto-assign public IPv4 when the switch is on", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(
      screen.getByRole("switch", {
        name: "Enable auto-assign public IPv4 address",
      }),
    )
    await user.click(screen.getByRole("button", { name: "Create Subnet" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalledTimes(2)
    })
    expect(sdk.send.mock.calls[1]?.[0]).toMatchObject({
      input: { SubnetId: "subnet-new", MapPublicIpOnLaunch: { Value: true } },
    })
  })
})
