import type { Address } from "@aws-sdk/client-ec2"
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

  return await mockRouter(importOriginal)
})

import { ElasticIpDetails } from "./elastic-ip-details"

function renderSettings(address: Address, onClose = vi.fn()) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "instances"], { Reservations: [] })
  renderWithClient(
    <ElasticIpDetails
      address={address}
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      tab="settings"
    />,
    qc,
  )

  return onClose
}

describe("elastic ip details", () => {
  it("keeps release disabled while the address is associated", () => {
    renderSettings({
      AllocationId: "eipalloc-1",
      AssociationId: "eipassoc-1",
      PublicIp: "198.51.100.7",
    })

    expect(
      screen.getByRole("button", { name: "Release Elastic IP" }),
    ).toBeDisabled()
  })

  it("releases a free address only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()

    const onClose = renderSettings({
      AllocationId: "eipalloc-1",
      PublicIp: "198.51.100.7",
    })

    await user.click(screen.getByRole("button", { name: "Release Elastic IP" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Release" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { AllocationId: "eipalloc-1" },
    })
  })
})
