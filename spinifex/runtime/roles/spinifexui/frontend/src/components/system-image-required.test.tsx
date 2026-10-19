import { DescribeImagesCommand } from "@aws-sdk/client-ec2"
import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { ec2ImagesQueryOptions } from "@/queries/ec2"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { SystemImageRequired } from "./system-image-required"

const importCommand = "spx admin images import --name spinifex-ecs-node"

function renderCallout(
  overrides: Partial<Parameters<typeof SystemImageRequired>[0]> = {},
) {
  const queryClient = createTestQueryClient()
  queryClient.setQueryData(ec2ImagesQueryOptions.queryKey, {
    $metadata: {},
    Images: [],
  })

  const { container } = renderWithClient(
    <SystemImageRequired
      description="Import it before creating a cluster."
      importCommand={importCommand}
      title="ECS system image not found"
      {...overrides}
    />,
    queryClient,
  )

  return { container }
}

describe("SystemImageRequired", () => {
  it("renders the title, description and import command", () => {
    renderCallout()

    expect(screen.getByText("ECS system image not found")).toBeInTheDocument()
    expect(
      screen.getByText("Import it before creating a cluster."),
    ).toBeInTheDocument()
    expect(screen.getByText(importCommand)).toBeInTheDocument()
  })

  it("copies the import command to the clipboard", async () => {
    const user = userEvent.setup()
    renderCallout()

    await user.click(screen.getByRole("button", { name: "Copy command" }))

    await expect(navigator.clipboard.readText()).resolves.toBe(importCommand)
  })

  it("refetches the image list when Recheck is clicked", async () => {
    mockSend.mockResolvedValue({ Images: [] })
    const user = userEvent.setup()
    renderCallout()

    await user.click(screen.getByRole("button", { name: /recheck/i }))

    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0]).toBeInstanceOf(DescribeImagesCommand)
  })

  it("disables Recheck while a recheck is in flight", async () => {
    // oxlint-disable-next-line promise/avoid-new -- holds the refetch in flight
    mockSend.mockReturnValue(new Promise(() => {}))
    const user = userEvent.setup()
    renderCallout()

    const recheck = screen.getByRole("button", { name: /recheck/i })
    await user.click(recheck)

    expect(recheck).toBeDisabled()
  })

  it("applies the call site's className alongside the base styles", () => {
    const { container } = renderCallout({ className: "mb-6" })

    expect(container.firstChild).toHaveClass("mb-6")
    expect(container.firstChild).toHaveClass("max-w-2xl")
  })

  it("omits call site spacing when no className is given", () => {
    const { container } = renderCallout()

    expect(container.firstChild).not.toHaveClass("mb-6")
  })
})
