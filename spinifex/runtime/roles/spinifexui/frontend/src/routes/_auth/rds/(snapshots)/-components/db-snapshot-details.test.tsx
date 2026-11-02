import type { DBSnapshot } from "@aws-sdk/client-rds"
import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
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

import { DBSnapshotDetails } from "./db-snapshot-details"

const ID = "orders-db-snapshot-20260817-1432"
const AUTOMATED_ID = "rds:orders-db-2026-08-17-03-00"
const ARN = `arn:aws:rds:ap-southeast-2:000000000000:snapshot:${ID}`

const SNAPSHOT: DBSnapshot = {
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
  Date: new Date("2026-08-17T14:47:10Z"),
  SourceIdentifier: ID,
  SourceType: "db-snapshot",
  EventCategories: ["backup", "notification"],
  Message: "Snapshot taken without quiescing the engine: crash consistent.",
}

interface SeedOptions {
  events?: unknown[]
  tags?: unknown[]
}

function seed(options: SeedOptions = {}): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "events", "db-snapshot", ID], {
    Events: options.events ?? [],
  })
  qc.setQueryData(["rds", "tags", ARN], { TagList: options.tags ?? [] })

  return qc
}

function renderDetails({
  snapshot = SNAPSHOT,
  tab = "overview",
  onClose = vi.fn(),
  qc = seed(),
}: {
  snapshot?: DBSnapshot
  tab?: PanelTab
  onClose?: () => void
  qc?: QueryClient
} = {}) {
  renderWithClient(
    <DBSnapshotDetails
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      snapshot={snapshot}
      tab={tab}
    />,
    qc,
  )
}

describe("DBSnapshotDetails", () => {
  it("renders the snapshot and the source it came from", () => {
    renderDetails()
    expect(screen.getByRole("heading", { name: ID })).toBeInTheDocument()
    expect(screen.getByText("100%")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "orders-db" })).toHaveAttribute(
      "href",
      "/rds/describe-db-instances/orders-db",
    )
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
    expect(screen.getByText("Encrypted — always on")).toBeInTheDocument()
    expect(screen.getByText("dbadmin")).toBeInTheDocument()
  })

  it("names what a restore takes from the snapshot rather than the form", () => {
    renderDetails()
    expect(
      screen.getByText(/the master credentials and the initial database/u),
    ).toBeInTheDocument()
  })

  it("holds both actions back while the snapshot is still being taken", () => {
    renderDetails({ snapshot: { ...SNAPSHOT, Status: "creating" } })
    expect(screen.getByRole("button", { name: "Restore" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled()
  })

  it("offers no delete for an automated backup and says why", () => {
    renderDetails({
      snapshot: {
        ...SNAPSHOT,
        DBSnapshotIdentifier: AUTOMATED_ID,
        SnapshotType: "automated",
      },
    })
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled()
    expect(
      screen.getByText(/deleted by the source instance's backup retention/u),
    ).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Restore" })).toBeInTheDocument()
  })

  it("links Restore to the restore form", () => {
    renderDetails()
    expect(screen.getByRole("link", { name: "Restore" })).toHaveAttribute(
      "href",
      `/rds/restore-db-instance-from-db-snapshot/${ID}`,
    )
  })

  it("asks before deleting", () => {
    renderDetails()
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    expect(mockSend).not.toHaveBeenCalled()
    expect(screen.getByRole("alertdialog")).toHaveTextContent(ID)
  })

  it("shows the empty state for events in the settings tab", () => {
    renderDetails({ tab: "settings" })
    expect(
      screen.getByText("No events in the last 14 days."),
    ).toBeInTheDocument()
  })

  // A snapshot the engine could not be quiesced for is still taken, and this
  // event is the only place that is reported.
  it("renders the crash-consistent notice from the snapshot's events", () => {
    renderDetails({
      tab: "settings",
      qc: seed({ events: [CRASH_CONSISTENT_EVENT] }),
    })
    expect(screen.getByText(/crash consistent/u)).toBeInTheDocument()
    expect(
      screen.getByText(formatDateTime(new Date("2026-08-17T14:47:10Z"))),
    ).toBeInTheDocument()
  })

  it("renders the tags the describe returned", async () => {
    renderDetails({
      tab: "settings",
      qc: seed({ tags: [{ Key: "env", Value: "prod" }] }),
    })
    await waitFor(() => {
      expect(screen.getByText("env")).toBeInTheDocument()
    })
    expect(screen.getByText("prod")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Edit tag env" }),
    ).toBeInTheDocument()
  })
})
