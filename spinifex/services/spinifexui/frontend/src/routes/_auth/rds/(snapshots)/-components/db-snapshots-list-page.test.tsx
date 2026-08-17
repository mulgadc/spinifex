import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => routerState.navigate,
    Link: ({
      children,
      to,
      params,
    }: {
      children: React.ReactNode
      to?: string
      params?: Record<string, string>
    }) => {
      let href = to ?? ""
      for (const [key, value] of Object.entries(params ?? {})) {
        href = href.replace(`$${key}`, value)
      }
      return <a href={href}>{children}</a>
    },
  }
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
  DBSnapshotIdentifier: "rds:orders-db-2026-08-17-03-00",
  DBInstanceIdentifier: "orders-db",
  SnapshotType: "automated",
  Status: "available",
  Engine: "postgres",
  EngineVersion: "18",
  AllocatedStorage: 20,
  SnapshotCreateTime: new Date("2026-08-17T03:00:00Z"),
}

const CREATING = {
  ...MANUAL,
  DBSnapshotIdentifier: "orders-db-snapshot-20260817-1500",
  Status: "creating",
}

function seed(snapshots: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbSnapshots"], { DBSnapshots: snapshots })
  qc.setQueryData(["rds", "dbInstances"], { DBInstances: [] })
  return qc
}

function rowFor(identifier: string): HTMLElement {
  const cell = screen.getByRole("link", { name: identifier })
  const row = cell.closest("tr")
  if (!row) {
    throw new Error(`no row for ${identifier}`)
  }
  return row
}

describe("DBSnapshotsListPage", () => {
  it("renders a snapshot row", () => {
    renderWithClient(<DBSnapshotsListPage />, seed([MANUAL]))
    expect(
      screen.getByRole("link", { name: "orders-db-snapshot-20260817-1432" }),
    ).toHaveAttribute(
      "href",
      "/rds/describe-db-snapshots/orders-db-snapshot-20260817-1432",
    )
    expect(screen.getByRole("link", { name: "orders-db" })).toHaveAttribute(
      "href",
      "/rds/describe-db-instances/orders-db",
    )
    expect(screen.getByText("postgres 18")).toBeInTheDocument()
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
    expect(screen.getByText("2026-08-17T14:32:00.000Z")).toBeInTheDocument()
  })

  it("explains the fallback when no snapshot exists", () => {
    renderWithClient(<DBSnapshotsListPage />, seed([]))
    expect(screen.getByText("No DB snapshots found.")).toBeInTheDocument()
  })

  it("narrows the table to the chosen snapshot type", () => {
    renderWithClient(<DBSnapshotsListPage />, seed([MANUAL, AUTOMATED]))
    fireEvent.click(screen.getByRole("button", { name: "Manual" }))
    expect(
      screen.getByRole("link", { name: "orders-db-snapshot-20260817-1432" }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole("link", { name: "rds:orders-db-2026-08-17-03-00" }),
    ).toBeNull()
  })

  // DeleteDBSnapshot refuses the rds: namespace outright, so the control is
  // never offered for one.
  it("offers no delete for an automated backup", () => {
    renderWithClient(<DBSnapshotsListPage />, seed([MANUAL, AUTOMATED]))
    const automated = rowFor("rds:orders-db-2026-08-17-03-00")
    const manual = rowFor("orders-db-snapshot-20260817-1432")
    expect(
      within(automated).getByRole("button", { name: "Delete" }),
    ).toBeDisabled()
    expect(within(manual).getByRole("button", { name: "Delete" })).toBeEnabled()
  })

  it("holds both actions back while the snapshot is still being taken", () => {
    renderWithClient(<DBSnapshotsListPage />, seed([CREATING]))
    const row = rowFor("orders-db-snapshot-20260817-1500")
    expect(within(row).getByRole("button", { name: "Restore" })).toBeDisabled()
    expect(within(row).getByRole("button", { name: "Delete" })).toBeDisabled()
  })

  it("navigates to the restore page from the row action", async () => {
    renderWithClient(<DBSnapshotsListPage />, seed([MANUAL]))
    fireEvent.click(screen.getByRole("button", { name: "Restore" }))
    await waitFor(() =>
      expect(routerState.navigate).toHaveBeenCalledWith({
        to: "/rds/restore-db-instance-from-db-snapshot/$id",
        params: { id: "orders-db-snapshot-20260817-1432" },
      }),
    )
  })

  it("opens the create dialog from the heading action", () => {
    renderWithClient(<DBSnapshotsListPage />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Take snapshot" }))
    expect(screen.getByText("Take DB snapshot")).toBeInTheDocument()
  })

  it("says why an automated backup cannot be removed by hand", () => {
    renderWithClient(<DBSnapshotsListPage />, seed([AUTOMATED]))
    expect(screen.getByText(/cannot be\s+deleted by hand/)).toBeInTheDocument()
  })
})
