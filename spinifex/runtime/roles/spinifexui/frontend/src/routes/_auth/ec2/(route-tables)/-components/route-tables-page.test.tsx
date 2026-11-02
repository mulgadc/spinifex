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

import { RouteTableDetails } from "./route-table-details"
import { RouteTablesPage } from "./route-tables-page"

const MAIN = {
  RouteTableId: "rtb-main",
  VpcId: "vpc-1",
  Associations: [{ RouteTableAssociationId: "rtbassoc-m", Main: true }],
  Routes: [{ DestinationCidrBlock: "10.0.0.0/16", GatewayId: "local" }],
}

const CUSTOM = {
  RouteTableId: "rtb-2",
  VpcId: "vpc-1",
  Tags: [{ Key: "Name", Value: "public" }],
  Associations: [
    { RouteTableAssociationId: "rtbassoc-1", SubnetId: "subnet-1" },
    { RouteTableAssociationId: "rtbassoc-2", SubnetId: "subnet-2" },
  ],
}

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "internetGateways"], { InternetGateways: [] })
  qc.setQueryData(["ec2", "natGateways"], { NatGateways: [] })
  qc.setQueryData(["ec2", "subnets"], { Subnets: [] })

  return qc
}

describe("RouteTablesPage", () => {
  it("opens the panel for a clicked row", async () => {
    const user = userEvent.setup()
    const onSearchChange = vi.fn()
    renderWithClient(
      <RouteTablesPage
        onSearchChange={onSearchChange}
        routeTables={[MAIN, CUSTOM]}
        search={CLOSED_PANEL}
      />,
      seed(),
    )

    await user.click(screen.getByRole("cell", { name: "2" }))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "rtb-2",
    })
  })

  it("shows the selected table's routes and associations", () => {
    renderWithClient(
      <RouteTablesPage
        onSearchChange={vi.fn()}
        routeTables={[MAIN, CUSTOM]}
        search={{ ...CLOSED_PANEL, selected: "rtb-2" }}
      />,
      seed(),
    )

    expect(
      screen.getByRole("region", { name: "Route table details" }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Disassociate subnet-2" }),
    ).toBeInTheDocument()
  })

  it("deletes each selected route table with its own call, main ones too", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <RouteTablesPage
        routeTables={[MAIN, CUSTOM]}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      seed(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select rtb-main" }))
    await user.click(screen.getByRole("checkbox", { name: "Select rtb-2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("rtb-main")).toBeInTheDocument()
    expect(within(dialog).getByText("public (rtb-2)")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(
          ([command]) => command.constructor.name === "DeleteRouteTableCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([{ RouteTableId: "rtb-main" }, { RouteTableId: "rtb-2" }])
  })
})

describe("RouteTableDetails", () => {
  it("lists a main table's implicit subnets without offering to disassociate them", () => {
    const qc = seed()
    qc.setQueryData(["ec2", "subnets"], {
      Subnets: [
        { SubnetId: "subnet-1", VpcId: "vpc-1" },
        { SubnetId: "subnet-3", VpcId: "vpc-1" },
        { SubnetId: "subnet-9", VpcId: "vpc-other" },
      ],
    })
    qc.setQueryData(["ec2", "routeTables"], { RouteTables: [MAIN, CUSTOM] })
    renderWithClient(
      <RouteTableDetails
        layout="page"
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        rtb={MAIN}
        tab="overview"
      />,
      qc,
    )

    const card = screen
      .getByRole("button", { name: /^Subnet associations/u })
      .closest(".rounded-lg")

    if (!(card instanceof HTMLElement)) {
      throw new TypeError("section not found")
    }

    const section = within(card)
    const item = section.getByRole("link", { name: "subnet-3" }).closest("li")

    expect(item).toHaveTextContent("Implicit")
    expect(section.queryByRole("link", { name: "subnet-1" })).toBeNull()
    expect(section.queryByRole("link", { name: "subnet-9" })).toBeNull()
    expect(section.queryByRole("button", { name: /^Disassociate/u })).toBeNull()
    expect(
      screen.getByRole("button", { name: "Subnet associations (1)" }),
    ).toBeInTheDocument()
  })

  it("lists no implicit subnets for a table that is not main", () => {
    const qc = seed()
    qc.setQueryData(["ec2", "subnets"], {
      Subnets: [{ SubnetId: "subnet-3", VpcId: "vpc-1" }],
    })
    qc.setQueryData(["ec2", "routeTables"], { RouteTables: [MAIN, CUSTOM] })
    renderWithClient(
      <RouteTableDetails
        layout="page"
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        rtb={CUSTOM}
        tab="overview"
      />,
      qc,
    )

    expect(screen.queryByText("Implicit")).toBeNull()
    expect(
      screen.getByRole("button", { name: /^Disassociate subnet-1/u }),
    ).toBeInTheDocument()
  })

  it("keeps a main route table's delete visible but disabled", () => {
    renderWithClient(
      <RouteTableDetails
        layout="page"
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        rtb={MAIN}
        tab="settings"
      />,
      seed(),
    )

    expect(
      screen.getByRole("button", { name: "Delete route table" }),
    ).toBeDisabled()
  })
})

describe("RouteTableDetails associations", () => {
  // A long list pushed the picker past the bottom of the panel.
  it("puts the subnet picker above the associations list", () => {
    const qc = seed()
    qc.setQueryData(["ec2", "routeTables"], { RouteTables: [MAIN, CUSTOM] })
    renderWithClient(
      <RouteTableDetails
        layout="page"
        onClose={vi.fn()}
        onTabChange={vi.fn()}
        rtb={CUSTOM}
        tab="overview"
      />,
      qc,
    )

    const picker = screen.getByRole("combobox", { name: "Associate subnet" })
    const firstAssociation = screen.getByRole("link", { name: "subnet-1" })

    expect(picker.compareDocumentPosition(firstAssociation)).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    )
  })
})

describe("RouteTablesPage main column", () => {
  it("shows main as a green pill and every other table as a grey no", () => {
    renderWithClient(
      <RouteTablesPage
        onSearchChange={vi.fn()}
        routeTables={[MAIN, CUSTOM]}
        search={CLOSED_PANEL}
      />,
      seed(),
    )

    expect(screen.getByText("main")).toHaveAttribute("data-tone", "success")
    expect(screen.getByText("no")).toHaveAttribute("data-tone", "muted")
  })
})
