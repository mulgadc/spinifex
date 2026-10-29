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

import { SubnetDetails } from "./subnet-details"
import { SubnetsPage } from "./subnets-page"

const SUBNET = {
  SubnetId: "subnet-1",
  VpcId: "vpc-1",
  CidrBlock: "10.0.1.0/24",
  AvailabilityZone: "ap-southeast-2a",
  State: "available" as const,
  Tags: [{ Key: "Name", Value: "public" }],
}

describe("SubnetsPage", () => {
  it("opens the panel for a clicked row", async () => {
    const user = userEvent.setup()
    const onSearchChange = vi.fn()
    renderWithClient(
      <SubnetsPage
        onSearchChange={onSearchChange}
        search={CLOSED_PANEL}
        subnets={[SUBNET]}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("cell", { name: "10.0.1.0/24" }))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "subnet-1",
    })
  })

  it("shows the selected subnet's details", () => {
    renderWithClient(
      <SubnetsPage
        onSearchChange={vi.fn()}
        search={{ ...CLOSED_PANEL, selected: "subnet-1" }}
        subnets={[SUBNET]}
      />,
      createTestQueryClient(),
    )

    const panel = within(screen.getByRole("region", { name: "Subnet details" }))
    expect(panel.getByRole("link", { name: "vpc-1" })).toHaveAttribute(
      "href",
      "/ec2/describe-vpcs/vpc-1",
    )
  })

  it("deletes each selected subnet with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <SubnetsPage
        subnets={[SUBNET, { ...SUBNET, SubnetId: "subnet-2", Tags: [] }]}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select subnet-1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select subnet-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("public (subnet-1)")).toBeInTheDocument()
    expect(within(dialog).getByText("subnet-2")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(
          ([command]) => command.constructor.name === "DeleteSubnetCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([{ SubnetId: "subnet-1" }, { SubnetId: "subnet-2" }])
  })
})

describe("SubnetDetails", () => {
  it("deletes only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderWithClient(
      <SubnetDetails
        onClose={onClose}
        onTabChange={vi.fn()}
        subnet={SUBNET}
        tab="settings"
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("button", { name: "Delete subnet" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { SubnetId: "subnet-1" },
    })
  })
})
