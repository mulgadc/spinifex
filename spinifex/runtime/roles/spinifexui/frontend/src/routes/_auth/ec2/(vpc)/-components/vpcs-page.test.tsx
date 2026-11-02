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

import { VpcsPage } from "./vpcs-page"

const ROWS = [
  {
    VpcId: "vpc-1",
    CidrBlock: "10.0.0.0/16",
    State: "available" as const,
    Tags: [{ Key: "Name", Value: "prod" }],
  },
  { VpcId: "vpc-2", CidrBlock: "10.1.0.0/16", State: "available" as const },
]

describe("VpcsPage", () => {
  it("deletes each selected VPC with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <VpcsPage vpcs={ROWS} onSearchChange={vi.fn()} search={CLOSED_PANEL} />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select vpc-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select vpc-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("prod (vpc-1)")).toBeInTheDocument()
    expect(within(dialog).getByText("vpc-2")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(([command]) => command.constructor.name === "DeleteVpcCommand")
        .map(([command]) => command.input),
    ).toStrictEqual([{ VpcId: "vpc-1" }, { VpcId: "vpc-2" }])
  })
})
