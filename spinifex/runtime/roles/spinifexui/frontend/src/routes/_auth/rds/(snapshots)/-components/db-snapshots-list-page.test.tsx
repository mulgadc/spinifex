import { type DBSnapshot, DeleteDBSnapshotCommand } from "@aws-sdk/client-rds"
import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
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

import { DBSnapshotsListPage } from "./db-snapshots-list-page"

const MANUAL = {
  DBSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
  DBInstanceIdentifier: "orders-db",
  SnapshotType: "manual",
  Status: "available",
  Engine: "postgres",
  EngineVersion: "18",
  AllocatedStorage: 20,
  SnapshotCreateTime: new Date("2026-08-17T14:32:00Z"),
}

const AUTOMATED = {
  ...MANUAL,
  DBSnapshotIdentifier: "rds:orders-db-2026-08-17-03-00",
  SnapshotType: "automated",
  SnapshotCreateTime: new Date("2026-08-17T03:00:00Z"),
}

function renderList(
  snapshots: DBSnapshot[],
  search: Partial<PanelSearch> = {},
  onSearchChange = vi.fn(),
  instance = "",
) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbInstances"], { DBInstances: [] })
  renderWithClient(
    <DBSnapshotsListPage
      instance={instance}
      onSearchChange={onSearchChange}
      search={{ ...CLOSED_PANEL, ...search }}
      snapshots={snapshots}
    />,
    qc,
  )

  return onSearchChange
}

describe("DBSnapshotsListPage", () => {
  it("renders a snapshot row", () => {
    renderList([MANUAL])
    expect(
      screen.getByRole("link", { name: "orders-db-snapshot-20260817-1432" }),
    ).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "orders-db" })).toHaveAttribute(
      "href",
      "/rds/describe-db-instances/orders-db",
    )
    expect(screen.getByText("postgres 18")).toBeInTheDocument()
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
    expect(
      screen.getByText(formatDateTime(new Date("2026-08-17T14:32:00Z"))),
    ).toBeInTheDocument()
  })

  it("explains the fallback when no snapshot exists", () => {
    renderList([])
    expect(screen.getByText("No DB snapshots found.")).toBeInTheDocument()
  })

  it("narrows the table to the chosen snapshot type", () => {
    renderList([MANUAL, AUTOMATED])
    fireEvent.click(screen.getByRole("button", { name: /^Manual/ }))
    expect(
      screen.getByRole("link", { name: "orders-db-snapshot-20260817-1432" }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole("link", { name: "rds:orders-db-2026-08-17-03-00" }),
    ).toBeNull()
  })

  it("counts each type beside its filter", () => {
    renderList([MANUAL, AUTOMATED])
    expect(screen.getByRole("group", { name: "Snapshot type" })).toBeVisible()
    expect(screen.getByRole("button", { name: "All 2" })).toHaveAttribute(
      "aria-pressed",
      "true",
    )
    expect(screen.getByRole("button", { name: "Manual 1" })).toBeVisible()
    expect(screen.getByRole("button", { name: "Automated 1" })).toBeVisible()
  })

  it("searches by identifier and DB instance", () => {
    renderList([
      MANUAL,
      {
        ...MANUAL,
        DBSnapshotIdentifier: "billing-final",
        DBInstanceIdentifier: "billing-db",
      },
    ])
    fireEvent.change(screen.getByLabelText("Filter snapshots"), {
      target: { value: "billing-db" },
    })
    expect(screen.getByRole("link", { name: "billing-final" })).toBeVisible()
    expect(
      screen.queryByRole("link", { name: "orders-db-snapshot-20260817-1432" }),
    ).toBeNull()
  })

  it("clears the search and the type together", () => {
    renderList([MANUAL, AUTOMATED])
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull()

    fireEvent.click(screen.getByRole("button", { name: /^Manual/ }))
    fireEvent.change(screen.getByLabelText("Filter snapshots"), {
      target: { value: "nothing" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }))

    expect(screen.getByLabelText("Filter snapshots")).toHaveValue("")
    expect(
      screen.getByRole("link", { name: "rds:orders-db-2026-08-17-03-00" }),
    ).toBeInTheDocument()
  })

  it("leaves the reserved namespace out of the page", () => {
    renderList([MANUAL, AUTOMATED])
    expect(screen.queryByText(/reserved rds: namespace/)).toBeNull()
  })

  it("selects the snapshot a row click opens", () => {
    const onSearchChange = renderList([MANUAL])
    fireEvent.click(screen.getByText("manual"))
    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "orders-db-snapshot-20260817-1432",
    })
  })

  it("keeps the panel open when a filter hides its row", () => {
    renderList([MANUAL, AUTOMATED], {
      selected: "rds:orders-db-2026-08-17-03-00",
    })
    fireEvent.click(screen.getByRole("button", { name: /^Manual/ }))
    expect(
      screen.getByRole("region", { name: "Snapshot details" }),
    ).toBeInTheDocument()
  })

  it("offers Take snapshot as a link from the heading", () => {
    renderList([])
    expect(
      screen.getByRole("link", { name: "Take snapshot" }),
    ).toBeInTheDocument()
  })

  it("renders the create form in the panel when asked", () => {
    renderList([], { create: true })
    expect(
      screen.getByRole("region", { name: "Take DB snapshot" }),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/No DB instance is available or stopped/),
    ).toBeInTheDocument()
  })

  it("fixes the instance when opened from a database", () => {
    renderList([], { create: true }, vi.fn(), "orders-db")
    expect(screen.queryByLabelText("DB instance")).toBeNull()
    expect(
      screen.getByLabelText<HTMLInputElement>("Snapshot identifier").value,
    ).toMatch(/^orders-db-snapshot-/)
  })

  it("deletes each selected DB snapshot with its own call", async () => {
    mockSend.mockClear()
    renderList([
      MANUAL,
      { ...MANUAL, DBSnapshotIdentifier: "orders-db-snapshot-20260818-0900" },
    ])
    const user = userEvent.setup()

    await user.click(
      screen.getByRole("checkbox", {
        name: "Select orders-db-snapshot-20260817-1432",
      }),
    )
    await user.click(
      screen.getByRole("checkbox", {
        name: "Select orders-db-snapshot-20260818-0900",
      }),
    )
    await user.click(screen.getByRole("button", { name: "Delete" }))

    const dialog = screen.getByRole("alertdialog")

    expect(
      within(dialog).getByText("Delete 2 DB snapshots?"),
    ).toBeInTheDocument()
    await user.type(
      await screen.findByLabelText(/^To confirm, type/u),
      "delete",
    )
    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    const deletes = () =>
      mockSend.mock.calls
        .map(([command]) => command as unknown)
        .filter((command) => command instanceof DeleteDBSnapshotCommand)
        .map((command) => command.input.DBSnapshotIdentifier)

    await waitFor(() => {
      expect(deletes()).toStrictEqual([
        "orders-db-snapshot-20260817-1432",
        "orders-db-snapshot-20260818-0900",
      ])
    })
  })
})
