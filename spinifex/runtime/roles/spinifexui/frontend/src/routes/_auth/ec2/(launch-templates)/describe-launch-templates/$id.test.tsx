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

  return await mockRouter(importOriginal, { params: { id: "lt-1" } })
})

import { Route } from "./$id"

async function renderPage() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "launchTemplates"], {
    LaunchTemplates: [
      {
        LaunchTemplateId: "lt-1",
        LaunchTemplateName: "web",
        DefaultVersionNumber: 1,
      },
    ],
  })
  qc.setQueryData(["ec2", "launchTemplates", "lt-1", "versions"], {
    LaunchTemplateVersions: [
      { VersionNumber: 2, LaunchTemplateData: {} },
      { VersionNumber: 1, DefaultVersion: true, LaunchTemplateData: {} },
    ],
  })
  // The new-version form below the table reads these.
  qc.setQueryData(["ec2", "images"], { Images: [] })
  qc.setQueryData(["ec2", "instances", "types"], { InstanceTypes: [] })
  qc.setQueryData(["ec2", "keypairs"], { KeyPairs: [] })
  qc.setQueryData(["ec2", "subnets"], { Subnets: [] })
  qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })
  const Detail = Route.options.component

  if (!Detail) {
    throw new Error("route has no component")
  }

  // The router plugin code-splits route components, so load it before render.
  await Detail.preload?.()
  renderWithClient(<Detail />, qc)
}

describe("launch template detail", () => {
  it("deletes a version only after it is confirmed", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("button", { name: "Delete version 2" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { LaunchTemplateId: "lt-1", Versions: ["2"] },
    })
  })

  it("does nothing when the dialog is cancelled", async () => {
    const user = userEvent.setup()
    await renderPage()

    await user.click(screen.getByRole("button", { name: "Delete version 2" }))
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(sdk.send).not.toHaveBeenCalled()
  })
})
