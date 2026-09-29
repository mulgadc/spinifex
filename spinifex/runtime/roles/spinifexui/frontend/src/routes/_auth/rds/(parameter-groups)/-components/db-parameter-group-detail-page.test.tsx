import type { DBParameterGroup } from "@aws-sdk/client-rds"
import { screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => vi.fn(),
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { DBParameterGroupDetailPage } from "./db-parameter-group-detail-page"

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

function seed(name: string, group: DBParameterGroup | null) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "parameterGroups", name], {
    DBParameterGroups: group ? [group] : [],
  })
  qc.setQueryData(["rds", "parameters", name], { Parameters: PARAMETERS })
  qc.setQueryData(["rds", "tags", `arn:${name}`], { TagList: [] })
  return qc
}

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

describe("DBParameterGroupDetailPage", () => {
  it("renders the parameters of a customer group", () => {
    renderWithClient(
      <DBParameterGroupDetailPage dbParameterGroupName="orders-pg" />,
      seed("orders-pg", CUSTOMER),
    )
    expect(screen.getByText("max_connections")).toBeInTheDocument()
    expect(
      screen.getByLabelText("Value of max_connections"),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /Delete/ })).toBeEnabled()
  })

  it("reports a group that does not exist", () => {
    renderWithClient(
      <DBParameterGroupDetailPage dbParameterGroupName="missing-pg" />,
      seed("missing-pg", null),
    )
    expect(
      screen.getByText("DB parameter group not found."),
    ).toBeInTheDocument()
  })

  // A default group is synthesised on describe and has no stored record, so
  // modify, delete and tagging would all be refused.
  it("renders a default group read-only, with no tags tab", () => {
    renderWithClient(
      <DBParameterGroupDetailPage dbParameterGroupName="default.postgres18" />,
      seed("default.postgres18", DEFAULT_GROUP),
    )
    expect(
      screen.getByText("This is a default parameter group"),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /Delete/ })).toBeDisabled()
    expect(screen.queryByLabelText("Value of max_connections")).toBeNull()
    expect(screen.queryByRole("tab", { name: "Tags" })).toBeNull()
  })

  it("offers the tags tab on a customer group", () => {
    renderWithClient(
      <DBParameterGroupDetailPage dbParameterGroupName="orders-pg" />,
      seed("orders-pg", CUSTOMER),
    )
    expect(screen.getByRole("tab", { name: "Tags" })).toBeInTheDocument()
  })
})
