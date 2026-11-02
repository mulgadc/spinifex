import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const send = vi.hoisted(() =>
  vi.fn(
    async (_command: { constructor: { name: string }; input: object }) => ({}),
  ),
)

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"

import { LaunchTemplatesPage } from "./launch-templates-page"

const TEMPLATES = [
  {
    LaunchTemplateId: "lt-1",
    LaunchTemplateName: "web",
    DefaultVersionNumber: 1,
    LatestVersionNumber: 3,
  },
  {
    LaunchTemplateId: "lt-2",
    LaunchTemplateName: "batch",
    DefaultVersionNumber: 2,
    LatestVersionNumber: 2,
  },
]

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "launchTemplates", "lt-1", "versions"], {
    LaunchTemplateVersions: [
      { VersionNumber: 3, VersionDescription: "bigger disk" },
      { VersionNumber: 1 },
    ],
  })

  return qc
}

describe("LaunchTemplatesPage", () => {
  it("opens the panel for a clicked row", async () => {
    const user = userEvent.setup()
    const onSearchChange = vi.fn()
    renderWithClient(
      <LaunchTemplatesPage
        launchTemplates={TEMPLATES}
        onSearchChange={onSearchChange}
        search={CLOSED_PANEL}
      />,
      seed(),
    )

    await user.click(screen.getByRole("cell", { name: "3" }))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "lt-1",
    })
  })

  it("lists the selected template's versions in its panel", async () => {
    renderWithClient(
      <LaunchTemplatesPage
        launchTemplates={TEMPLATES}
        onSearchChange={vi.fn()}
        search={{ ...CLOSED_PANEL, selected: "lt-1" }}
      />,
      seed(),
    )

    const panel = screen.getByRole("region", {
      name: "Launch template details",
    })

    expect(panel).toHaveTextContent("default v1")
    await expect(screen.findByText(/bigger disk/u)).resolves.toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Set version 1 as default" }),
    ).toBeDisabled()
  })

  it("deletes each selected template with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <LaunchTemplatesPage
        launchTemplates={TEMPLATES}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      seed(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select lt-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select lt-2" }))

    const bar = screen.getByRole("toolbar", { name: "Selection actions" })

    await user.click(within(bar).getByRole("button", { name: "Delete" }))

    const dialog = screen.getByRole("alertdialog")

    expect(
      within(dialog).getByRole("list", { name: "Selected launch templates" }),
    ).toHaveTextContent("batch (lt-2)")
    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    expect(
      send.mock.calls.map(([command]) => [
        command.constructor.name,
        command.input,
      ]),
    ).toStrictEqual([
      ["DeleteLaunchTemplateCommand", { LaunchTemplateId: "lt-1" }],
      ["DeleteLaunchTemplateCommand", { LaunchTemplateId: "lt-2" }],
    ])
  })
})
