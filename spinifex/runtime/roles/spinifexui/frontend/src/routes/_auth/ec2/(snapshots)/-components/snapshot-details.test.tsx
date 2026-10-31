import type { Snapshot } from "@aws-sdk/client-ec2"
import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk, navigate } = vi.hoisted(() => {
  interface SdkCommand {
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({})),
    },
    navigate: vi.fn(),
  }
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { navigate })
})

import { SnapshotDetails } from "./snapshot-details"

function renderSnapshot(snapshot: Snapshot) {
  sdk.send.mockClear()
  navigate.mockClear()
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "regions"], {
    Regions: [{ RegionName: "ap-southeast-2" }],
  })
  renderWithClient(
    <SnapshotDetails
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      snapshot={snapshot}
      tab="settings"
    />,
    qc,
  )
}

describe("SnapshotDetails", () => {
  it("keeps copy and register disabled until the snapshot completes", () => {
    renderSnapshot({ SnapshotId: "snap-1", State: "pending" })

    for (const name of ["Copy snapshot", "Register as AMI"]) {
      expect(screen.getByRole("button", { name })).toBeDisabled()
    }
  })

  it("sends a pending snapshot's delete and shows the refusal", async () => {
    const user = userEvent.setup()
    renderSnapshot({ SnapshotId: "snap-1", State: "pending" })
    sdk.send.mockRejectedValueOnce(
      new Error("The snapshot snap-1 is currently in use."),
    )

    await user.click(screen.getByRole("button", { name: "Delete snapshot" }))
    await user.type(
      await screen.findByLabelText(/^To confirm, type/u),
      "delete",
    )
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await expect(
      screen.findByText("The snapshot snap-1 is currently in use."),
    ).resolves.toBeInTheDocument()
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { SnapshotId: "snap-1" },
    })
  })

  it("copies a completed snapshot within the cluster's region", async () => {
    const user = userEvent.setup()
    renderSnapshot({ SnapshotId: "snap-1", State: "completed" })
    sdk.send.mockResolvedValueOnce({ SnapshotId: "snap-2" })

    await user.click(screen.getByRole("button", { name: "Copy snapshot" }))
    const form = screen.getByRole("region", { name: "Copy snapshot" })
    await user.type(
      within(form).getByLabelText("Description (optional)"),
      "backup",
    )
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(
      within(form).getByRole("button", { name: "Copy snapshot" }),
    )

    await waitFor(() => {
      expect(navigate).toHaveBeenCalledWith({
        to: "/ec2/describe-snapshots",
        search: { selected: "snap-2" },
      })
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: {
        SourceSnapshotId: "snap-1",
        SourceRegion: "ap-southeast-2",
        Description: "backup",
      },
    })
    expect(screen.queryByRole("region", { name: "Copy snapshot" })).toBeNull()
  })

  it("selects the registered AMI in the image list", async () => {
    const user = userEvent.setup()
    renderSnapshot({ SnapshotId: "snap-1", State: "completed" })
    sdk.send.mockResolvedValueOnce({ ImageId: "ami-123" })

    await user.click(screen.getByRole("button", { name: "Register as AMI" }))
    const form = screen.getByRole("region", { name: "Register as AMI" })
    await user.type(within(form).getByLabelText("Name"), "my-image")
    await user.click(within(form).getByRole("button", { name: "Register AMI" }))

    await waitFor(() => {
      expect(navigate).toHaveBeenCalledWith({
        to: "/ec2/describe-images",
        search: { selected: "ami-123" },
      })
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: {
        Name: "my-image",
        BlockDeviceMappings: [
          { DeviceName: "/dev/sda1", Ebs: { SnapshotId: "snap-1" } },
        ],
      },
    })
  })

  it("keeps the form open with the refusal beside the button", async () => {
    const user = userEvent.setup()
    renderSnapshot({ SnapshotId: "snap-1", State: "completed" })
    sdk.send.mockRejectedValueOnce(new Error("InvalidAMIName.Duplicate"))

    await user.click(screen.getByRole("button", { name: "Register as AMI" }))
    const form = screen.getByRole("region", { name: "Register as AMI" })
    await user.type(within(form).getByLabelText("Name"), "taken")
    await user.click(within(form).getByRole("button", { name: "Register AMI" }))

    await expect(
      within(form).findByText("InvalidAMIName.Duplicate"),
    ).resolves.toBeInTheDocument()
    expect(navigate).not.toHaveBeenCalled()
  })
})
