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

  return await mockRouter(importOriginal, { params: { id: "igw-1" } })
})

import { Route } from "./$id"

async function renderPage() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "internetGateways", "igw-1"], {
    InternetGateways: [
      {
        InternetGatewayId: "igw-1",
        Attachments: [{ VpcId: "vpc-1", State: "available" }],
      },
    ],
  })
  qc.setQueryData(["ec2", "vpcs"], { Vpcs: [] })
  const Detail = Route.options.component

  if (!Detail) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, qc)
}

describe("internet gateway detail", () => {
  it("detaches only after it is confirmed", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("button", { name: "Detach" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Detach" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { InternetGatewayId: "igw-1", VpcId: "vpc-1" },
    })
  })

  it("does nothing when the dialog is cancelled", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("button", { name: "Detach" }))
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(sdk.send).not.toHaveBeenCalled()
  })
})
