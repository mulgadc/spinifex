import type { Volume } from "@aws-sdk/client-ec2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
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

import { VolumeDetails } from "./volume-details"

const IN_USE: Volume = {
  VolumeId: "vol-1",
  Size: 8,
  State: "in-use",
  Attachments: [
    {
      VolumeId: "vol-1",
      InstanceId: "i-1",
      Device: "/dev/sdf",
      State: "attached",
    },
  ],
}

const AVAILABLE: Volume = {
  VolumeId: "vol-2",
  Size: 20,
  State: "available",
  Attachments: [],
}

function renderVolume(volume: Volume, tab: PanelTab = "overview") {
  sdk.send.mockClear()
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "instances"], {
    Reservations: [
      { Instances: [{ InstanceId: "i-9", State: { Name: "running" } }] },
    ],
  })
  renderWithClient(
    <VolumeDetails
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab={tab}
      volume={volume}
    />,
    qc,
  )
}

describe("VolumeDetails", () => {
  it("offers detach, not attach, for an attached volume", () => {
    renderVolume(IN_USE)

    expect(screen.getByRole("button", { name: "Attach" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Detach" })).toBeEnabled()
    expect(screen.getByRole("link", { name: "i-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-instances/i-1",
    )
  })

  it("force-detaches from the attached instance only once confirmed", async () => {
    const user = userEvent.setup()
    renderVolume(IN_USE)

    await user.click(screen.getByRole("button", { name: "Detach" }))
    await user.click(screen.getByRole("switch", { name: "Force detach" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Detach" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { VolumeId: "vol-1", InstanceId: "i-1", Force: true },
    })
  })

  it("attaches an available volume to the chosen running instance", async () => {
    const user = userEvent.setup()
    renderVolume(AVAILABLE)

    await user.click(screen.getByRole("button", { name: "Attach" }))
    await user.click(screen.getByLabelText("Instance"))
    await user.click(await screen.findByRole("option", { name: "i-9" }))
    await user.click(screen.getByRole("button", { name: "Attach" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { VolumeId: "vol-2", InstanceId: "i-9" },
    })
  })

  it("keeps resize and delete visible but disabled while attached", () => {
    renderVolume(IN_USE, "settings")

    expect(screen.getByRole("button", { name: "Resize" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Delete volume" })).toBeDisabled()
  })

  it("links an available volume to its resize page", () => {
    renderVolume(AVAILABLE, "settings")

    expect(screen.getByRole("link", { name: "Resize" })).toHaveAttribute(
      "href",
      "/ec2/modify-volume/vol-2",
    )
    expect(screen.getByRole("button", { name: "Delete volume" })).toBeEnabled()
  })
})
