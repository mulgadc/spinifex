import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen } from "@testing-library/react"
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

import { DBSnapshotDetailPage } from "./db-snapshot-detail-page"

const ID = "orders-db-snapshot-20260817-1432"
const AUTOMATED_ID = "rds:orders-db-2026-08-17-03-00"
const ARN = `arn:aws:rds:ap-southeast-2:000000000000:snapshot:${ID}`

const SNAPSHOT = {
  DBSnapshotIdentifier: ID,
  DBSnapshotArn: ARN,
  DBInstanceIdentifier: "orders-db",
  SnapshotType: "manual",
  Status: "available",
  PercentProgress: 100,
  Engine: "postgres",
  EngineVersion: "18",
  AllocatedStorage: 20,
  StorageType: "gp3",
  Encrypted: true,
  MasterUsername: "dbadmin",
  Port: 5432,
  VpcId: "vpc-1",
  SnapshotCreateTime: new Date("2026-08-17T14:32:00Z"),
}

const CRASH_CONSISTENT_EVENT = {
  Date: new Date("2026-08-17T14:32:10Z"),
  SourceIdentifier: ID,
  SourceType: "db-snapshot",
  EventCategories: ["backup", "notification"],
  Message: "Snapshot taken without quiescing the engine: crash consistent.",
}

interface SeedOptions {
  id?: string
  snapshots?: unknown[]
  events?: unknown[]
  tags?: unknown[]
}

function seed(options: SeedOptions = {}): QueryClient {
  const id = options.id ?? ID
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbSnapshots", id], {
    DBSnapshots: options.snapshots ?? [SNAPSHOT],
  })
  qc.setQueryData(["rds", "events", "db-snapshot", id], {
    Events: options.events ?? [],
  })
  qc.setQueryData(["rds", "tags", ARN], { TagList: options.tags ?? [] })
  qc.setQueryData(["rds", "tags", ""], { TagList: [] })
  return qc
}

function openTab(name: string) {
  fireEvent.click(screen.getByRole("tab", { name }))
}

describe("DBSnapshotDetailPage", () => {
  it("renders the snapshot and the source it came from", () => {
    renderWithClient(<DBSnapshotDetailPage dbSnapshotIdentifier={ID} />, seed())
    expect(screen.getByText(ID)).toBeInTheDocument()
    expect(screen.getByText("100%")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "orders-db" })).toHaveAttribute(
      "href",
      "/rds/describe-db-instances/orders-db",
    )
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
    expect(screen.getByText("Encrypted — always on")).toBeInTheDocument()
    expect(screen.getByText("dbadmin")).toBeInTheDocument()
  })

  it("reports a missing snapshot rather than rendering an empty shell", () => {
    renderWithClient(
      <DBSnapshotDetailPage dbSnapshotIdentifier={ID} />,
      seed({ snapshots: [] }),
    )
    expect(screen.getByText("DB snapshot not found.")).toBeInTheDocument()
  })

  it("names what a restore takes from the snapshot rather than the form", () => {
    renderWithClient(<DBSnapshotDetailPage dbSnapshotIdentifier={ID} />, seed())
    expect(
      screen.getByText(/the master credentials and the initial database/),
    ).toBeInTheDocument()
  })

  it("holds both actions back while the snapshot is still being taken", () => {
    renderWithClient(
      <DBSnapshotDetailPage dbSnapshotIdentifier={ID} />,
      seed({ snapshots: [{ ...SNAPSHOT, Status: "creating" }] }),
    )
    expect(screen.getByRole("button", { name: "Restore" })).toBeDisabled()
    expect(screen.getByRole("button", { name: /Delete/ })).toBeDisabled()
  })

  it("offers no delete for an automated backup and says why", () => {
    renderWithClient(
      <DBSnapshotDetailPage dbSnapshotIdentifier={AUTOMATED_ID} />,
      seed({
        id: AUTOMATED_ID,
        snapshots: [
          {
            ...SNAPSHOT,
            DBSnapshotIdentifier: AUTOMATED_ID,
            SnapshotType: "automated",
          },
        ],
      }),
    )
    expect(screen.getByRole("button", { name: /Delete/ })).toBeDisabled()
    expect(
      screen.getByText(/deleted by the source instance's backup retention/),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Restore" })).toBeEnabled()
  })

  it("navigates to the restore page from the heading action", () => {
    renderWithClient(<DBSnapshotDetailPage dbSnapshotIdentifier={ID} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Restore" }))
    expect(routerState.navigate).toHaveBeenCalledWith({
      to: "/rds/restore-db-instance-from-db-snapshot/$id",
      params: { id: ID },
    })
  })

  it("shows the empty state on the events tab", () => {
    renderWithClient(<DBSnapshotDetailPage dbSnapshotIdentifier={ID} />, seed())
    openTab("Events")
    expect(
      screen.getByText("No events in the last 14 days."),
    ).toBeInTheDocument()
  })

  // A snapshot the engine could not be quiesced for is still taken, and this
  // event is the only place that is reported.
  it("renders the crash-consistent notice from the snapshot's events", () => {
    renderWithClient(
      <DBSnapshotDetailPage dbSnapshotIdentifier={ID} />,
      seed({ events: [CRASH_CONSISTENT_EVENT] }),
    )
    openTab("Events")
    expect(screen.getByText(/crash consistent/)).toBeInTheDocument()
    expect(screen.getByText("2026-08-17T14:32:10.000Z")).toBeInTheDocument()
  })

  it("renders the tags the describe returned", () => {
    renderWithClient(
      <DBSnapshotDetailPage dbSnapshotIdentifier={ID} />,
      seed({ tags: [{ Key: "env", Value: "prod" }] }),
    )
    openTab("Tags")
    expect(screen.getByDisplayValue("env")).toBeInTheDocument()
    expect(screen.getByDisplayValue("prod")).toBeInTheDocument()
  })
})
