import type { Snapshot } from "@aws-sdk/client-ec2"
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

import { SnapshotDetails } from "./snapshot-details"

function renderSnapshot(snapshot: Snapshot) {
  sdk.send.mockClear()
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

    await user.click(screen.getByRole("button", { name: "Copy snapshot" }))
    await user.type(screen.getByLabelText("Description (optional)"), "backup")
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Copy snapshot" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: {
        SourceSnapshotId: "snap-1",
        SourceRegion: "ap-southeast-2",
        Description: "backup",
      },
    })
  })

  it("links the registered AMI instead of closing", async () => {
    const user = userEvent.setup()
    renderSnapshot({ SnapshotId: "snap-1", State: "completed" })
    sdk.send.mockResolvedValueOnce({ ImageId: "ami-123" })

    await user.click(screen.getByRole("button", { name: "Register as AMI" }))
    await user.type(screen.getByLabelText("Name"), "my-image")
    await user.click(screen.getByRole("button", { name: "Register AMI" }))

    await expect(
      screen.findByRole("link", { name: "ami-123" }),
    ).resolves.toHaveAttribute("href", "/ec2/describe-images/ami-123")
  })
})
