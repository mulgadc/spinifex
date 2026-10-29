import type { DBParameterGroup } from "@aws-sdk/client-rds"
import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { DBParameterGroupDetails } from "./db-parameter-group-details"

const PARAMETERS = [
  {
    ParameterName: "max_connections",
    ParameterValue: "200",
    Source: "user",
    ApplyType: "static",
    ApplyMethod: "pending-reboot",
    DataType: "integer",
    AllowedValues: "6-8388607",
    IsModifiable: true,
  },
  {
    ParameterName: "ssl",
    ParameterValue: "1",
    Source: "engine-default",
    ApplyType: "static",
    ApplyMethod: "pending-reboot",
    DataType: "boolean",
    AllowedValues: "0,1",
    IsModifiable: false,
  },
]

const CUSTOMER = {
  DBParameterGroupName: "orders-pg",
  DBParameterGroupFamily: "postgres18",
  Description: "Tuned settings",
  DBParameterGroupArn: "arn:orders-pg",
}

const DEFAULT_GROUP = {
  DBParameterGroupName: "default.postgres18",
  DBParameterGroupFamily: "postgres18",
  Description: "Default parameter group for postgres18",
  DBParameterGroupArn: "arn:default.postgres18",
}

function renderDetails(
  group: DBParameterGroup,
  tab: PanelTab,
  layout: "auto" | "page" = "auto",
) {
  const name = group.DBParameterGroupName ?? ""
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "parameters", name], { Parameters: PARAMETERS })
  qc.setQueryData(["rds", "tags", `arn:${name}`], { TagList: [] })
  renderWithClient(
    <DBParameterGroupDetails
      group={group}
      layout={layout}
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    qc,
  )
}

describe("DBParameterGroupDetails", () => {
  it("lists only the overridden values in the overview", () => {
    renderDetails(CUSTOMER, "overview")
    expect(screen.getByText("Overridden values (1)")).toBeInTheDocument()
    expect(screen.getByText("max_connections")).toBeInTheDocument()
    expect(screen.queryByText("ssl")).toBeNull()
  })

  // The editor's table does not fit the side panel, so the panel hands off to
  // the page rather than offering a cramped copy of it.
  it("links the side panel to the page for editing", () => {
    renderDetails(CUSTOMER, "settings")
    expect(screen.queryByLabelText("Value of max_connections")).toBeNull()
    expect(
      screen.getByRole("link", { name: "Edit parameters" }),
    ).toHaveAttribute("href", "/rds/describe-db-parameter-groups/orders-pg")
  })

  it("edits parameters on the group's page", () => {
    renderDetails(CUSTOMER, "settings", "page")
    expect(
      screen.getByLabelText("Value of max_connections"),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: /Delete parameter group/ }),
    ).toBeEnabled()
  })

  it("names the in-use refusal in the delete dialog", () => {
    renderDetails(CUSTOMER, "settings")
    fireEvent.click(
      screen.getByRole("button", { name: /Delete parameter group/ }),
    )
    expect(
      screen.getByText(/refused while any DB instance references the group/),
    ).toBeInTheDocument()
  })

  // A default group is synthesised on describe and has no stored record, so
  // modify, delete and tagging would all be refused.
  it("renders a default group read-only and untaggable", () => {
    renderDetails(DEFAULT_GROUP, "settings", "page")
    expect(
      screen.getByRole("button", { name: /Delete parameter group/ }),
    ).toBeDisabled()
    expect(screen.queryByLabelText("Value of max_connections")).toBeNull()
    expect(
      screen.getByText("A default group cannot be tagged."),
    ).toBeInTheDocument()
  })

  it("explains a default group in its overview", () => {
    renderDetails(DEFAULT_GROUP, "overview")
    expect(
      screen.getByText("This is a default parameter group"),
    ).toBeInTheDocument()
  })
})
