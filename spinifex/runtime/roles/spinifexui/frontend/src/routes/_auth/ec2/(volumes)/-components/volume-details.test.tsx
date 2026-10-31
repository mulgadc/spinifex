import type { Volume } from "@aws-sdk/client-ec2"
import { screen, waitFor, within } from "@testing-library/react"
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
  AvailabilityZone: "us-east-1a",
  Attachments: [],
}

const INSTANCES = [
  {
    InstanceId: "i-9",
    State: { Name: "running" },
    Placement: { AvailabilityZone: "us-east-1a" },
  },
  {
    InstanceId: "i-stopped",
    State: { Name: "stopped" },
    Placement: { AvailabilityZone: "us-east-1a" },
  },
  {
    InstanceId: "i-pending",
    State: { Name: "pending" },
    Placement: { AvailabilityZone: "us-east-1a" },
  },
  {
    InstanceId: "i-elsewhere",
    State: { Name: "running" },
    Placement: { AvailabilityZone: "us-east-1b" },
  },
]

async function openAttachForm(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: "Attach" }))

  return within(screen.getByRole("region", { name: "Attach volume" }))
}

function renderVolume(
  volume: Volume,
  tab: PanelTab = "overview",
  instances = INSTANCES,
) {
  sdk.send.mockClear()
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "instances"], {
    Reservations: [{ Instances: instances }],
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

    const form = await openAttachForm(user)
    await user.click(form.getByLabelText("Instance"))
    await user.click(await screen.findByRole("option", { name: "i-9" }))
    await user.click(form.getByRole("button", { name: "Attach" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { VolumeId: "vol-2", InstanceId: "i-9" },
    })
    await waitFor(() => {
      expect(
        screen.queryByRole("region", { name: "Attach volume" }),
      ).not.toBeInTheDocument()
    })
  })

  it("offers running and stopped instances in the volume's zone only", async () => {
    const user = userEvent.setup()
    renderVolume(AVAILABLE)

    const form = await openAttachForm(user)
    await user.click(form.getByLabelText("Instance"))
    await screen.findByRole("option", { name: "i-9" })

    expect(
      screen.getAllByRole("option").map((option) => option.textContent),
    ).toStrictEqual(["i-9", "i-stopped"])
  })

  it("says why there is nothing to attach to", async () => {
    const user = userEvent.setup()
    renderVolume(AVAILABLE, "overview", INSTANCES.slice(2))

    const form = await openAttachForm(user)

    await expect(
      form.findByText("No running or stopped instances in us-east-1a."),
    ).resolves.toBeInTheDocument()
    expect(form.getByRole("button", { name: "Attach" })).toBeDisabled()
  })

  it("keeps resize and delete visible but disabled while attached", () => {
    renderVolume(IN_USE, "settings")

    expect(screen.getByRole("button", { name: "Resize" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Delete volume" })).toBeDisabled()
  })

  it("resizes an available volume from its settings", async () => {
    const user = userEvent.setup()
    renderVolume(AVAILABLE, "settings")

    expect(screen.getByRole("button", { name: "Delete volume" })).toBeEnabled()
    await user.click(screen.getByRole("button", { name: "Resize" }))
    const form = within(screen.getByRole("region", { name: "Resize volume" }))
    const size = form.getByLabelText("New size (GiB)")
    await user.clear(size)
    await user.type(size, "50")
    await user.click(form.getByRole("button", { name: "Resize volume" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { VolumeId: "vol-2", Size: 50 },
    })
  })
})
