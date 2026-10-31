import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly constructor: { name: string }
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

import { CLOSED_PANEL } from "@/lib/panel-search"

import { SecurityGroupsPage } from "./security-groups-page"

const ROWS = [
  { GroupId: "sg-1", GroupName: "web", VpcId: "vpc-1" },
  { GroupId: "sg-2", GroupName: "db", VpcId: "vpc-1" },
]

describe("SecurityGroupsPage", () => {
  it("deletes each selected security group with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <SecurityGroupsPage
        securityGroups={ROWS}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select sg-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select sg-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("web (sg-1)")).toBeInTheDocument()
    expect(within(dialog).getByText("db (sg-2)")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(
          ([command]) =>
            command.constructor.name === "DeleteSecurityGroupCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([{ GroupId: "sg-1" }, { GroupId: "sg-2" }])
  })
})
