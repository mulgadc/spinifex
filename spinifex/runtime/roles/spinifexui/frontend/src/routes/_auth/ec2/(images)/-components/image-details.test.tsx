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

import type { PanelTab } from "@/lib/panel-search"

import { ImageDetails } from "./image-details"

const IMAGE = {
  ImageId: "ami-1",
  Name: "ubuntu-26.04",
  State: "available" as const,
  OwnerId: "000000000001",
  BlockDeviceMappings: [
    { DeviceName: "/dev/vda", Ebs: { SnapshotId: "snap-1", VolumeSize: 8 } },
  ],
}

function renderDetails(tab: PanelTab, onClose = vi.fn()) {
  renderWithClient(
    <ImageDetails
      image={IMAGE}
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    createTestQueryClient(),
  )
}

describe("ImageDetails", () => {
  it("links the block device's snapshot", () => {
    renderDetails("overview")

    expect(screen.getByRole("link", { name: "snap-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-snapshots/snap-1",
    )
  })

  it("deregisters only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderDetails("settings", onClose)

    await user.click(screen.getByRole("button", { name: "Deregister image" }))
    expect(sdk.send).not.toHaveBeenCalledWith(
      expect.objectContaining({ input: { ImageId: "ami-1" } }),
    )
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send).toHaveBeenCalledWith(
      expect.objectContaining({ input: { ImageId: "ami-1" } }),
    )
  })
})
