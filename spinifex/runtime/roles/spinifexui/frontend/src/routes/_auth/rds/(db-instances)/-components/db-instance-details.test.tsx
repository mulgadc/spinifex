import { type QueryClient, useSuspenseQuery } from "@tanstack/react-query"
import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { rdsDBInstanceQueryOptions } from "@/queries/rds"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))
const mockNavigate = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { navigate: mockNavigate })
})

import { DBInstanceDetails } from "./db-instance-details"

// Reads the instance the way its page does, so a refetch reaches the panel.
function Details({
  onClose = vi.fn(),
  tab = "overview",
}: {
  onClose?: () => void
  tab?: PanelTab
}) {
  const { data } = useSuspenseQuery(rdsDBInstanceQueryOptions("orders-db"))
  const instance = data.DBInstances?.[0]

  return instance ? (
    <DBInstanceDetails
      instance={instance}
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      tab={tab}
    />
  ) : null
}

const ARN = "arn:aws:rds:ap-southeast-2:000000000000:db:orders-db"

const INSTANCE = {
  DBInstanceIdentifier: "orders-db",
  DBInstanceArn: ARN,
  DBInstanceStatus: "available",
  Engine: "postgres",
  EngineVersion: "18",
  DBInstanceClass: "db.t3.micro",
  AllocatedStorage: 20,
  StorageType: "gp3",
  StorageEncrypted: true,
  MasterUsername: "dbadmin",
  DBName: "orders",
  DeletionProtection: false,
  BackupRetentionPeriod: 7,
  PreferredBackupWindow: "03:00-03:30",
  PreferredMaintenanceWindow: "sun:04:00-sun:04:30",
  Endpoint: { Address: "orders-db.rds.internal", Port: 5432 },
  DBSubnetGroup: { DBSubnetGroupName: "db-subnets", VpcId: "vpc-1" },
  VpcSecurityGroups: [{ VpcSecurityGroupId: "sg-1", Status: "active" }],
  DBParameterGroups: [
    {
      DBParameterGroupName: "default.postgres18",
      ParameterApplyStatus: "in-sync",
    },
  ],
}

const BACKUP_FAILURE_EVENT = {
  Date: new Date("2026-08-16T03:05:00Z"),
  SourceIdentifier: "orders-db",
  SourceType: "db-instance",
  EventCategories: ["backup", "failure"],
  Message: "The automated backup could not be taken: volume busy",
}

const BACKUP_CREATED_EVENT = {
  Date: new Date("2026-08-15T03:02:00Z"),
  SourceIdentifier: "orders-db",
  SourceType: "db-instance",
  EventCategories: ["backup", "configuration-change"],
  Message: "The backup window moved.",
}

const AUTOMATED_SNAPSHOTS = [
  {
    DBSnapshotIdentifier: "rds:orders-db-2026-08-14-03-02",
    SnapshotType: "automated",
    Status: "available",
    SnapshotCreateTime: new Date("2026-08-14T03:02:00Z"),
  },
  {
    DBSnapshotIdentifier: "rds:orders-db-2026-08-15-03-02",
    SnapshotType: "automated",
    Status: "available",
    SnapshotCreateTime: new Date("2026-08-15T03:02:00Z"),
  },
]

interface SeedOptions {
  instances?: unknown[]
  events?: unknown[]
  tags?: unknown[]
  snapshots?: unknown[]
  automatedBackups?: unknown[]
}

function seed(options: SeedOptions = {}): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbInstances", "orders-db"], {
    DBInstances: options.instances ?? [INSTANCE],
  })
  qc.setQueryData(["rds", "events", "db-instance", "orders-db"], {
    Events: options.events ?? [],
  })
  qc.setQueryData(["rds", "dbSnapshots", "instance", "orders-db"], {
    DBSnapshots: options.snapshots ?? [],
  })
  qc.setQueryData(["rds", "automatedBackups", "orders-db"], {
    DBInstanceAutomatedBackups: options.automatedBackups ?? [],
  })
  qc.setQueryData(["rds", "tags", ARN], { TagList: options.tags ?? [] })
  qc.setQueryData(["rds", "parameterGroups"], { DBParameterGroups: [] })
  qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })
  qc.setQueryData(["rds", "orderableOptions", "postgres"], {
    OrderableDBInstanceOptions: [{ DBInstanceClass: "db.t3.micro" }],
  })

  return qc
}

const SNAPSHOT = {
  DBSnapshotIdentifier: "orders-db-snapshot-20260817-1432",
  DBInstanceIdentifier: "orders-db",
  SnapshotType: "manual",
  Status: "available",
  SnapshotCreateTime: new Date("2026-08-17T14:32:00Z"),
}

const AUTOMATED_SNAPSHOT = {
  ...SNAPSHOT,
  DBSnapshotIdentifier: "rds:orders-db-2026-08-17-03-00",
  SnapshotType: "automated",
}

function rowFor(identifier: string): HTMLElement {
  const row = screen.getByText(identifier).closest("li")

  if (!row) {
    throw new Error(`no row for ${identifier}`)
  }

  return row
}

async function chooseAction(name: string) {
  const user = userEvent.setup()
  await user.click(screen.getByRole("button", { name: "Actions" }))
  await user.click(await screen.findByRole("menuitem", { name }))
}

describe("DBInstanceDetails", () => {
  beforeEach(() => {
    mockSend.mockResolvedValue({})
  })

  it("renders the identifier and status", () => {
    renderWithClient(<Details />, seed())
    expect(screen.getByText("orders-db")).toBeInTheDocument()
    expect(screen.getByText("available")).toBeInTheDocument()
  })

  it("shows the endpoint on the connectivity tab", () => {
    renderWithClient(<Details />, seed())
    expect(screen.getByText("orders-db.rds.internal")).toBeInTheDocument()
    expect(screen.getByText("5432")).toBeInTheDocument()
    expect(screen.getByText("No — private VPC address")).toBeInTheDocument()
  })

  it("offers a ready-to-paste connect command for the engine", () => {
    renderWithClient(<Details />, seed())
    fireEvent.click(screen.getByRole("button", { name: "AWS CLI" }))
    expect(screen.getByText("psql")).toBeInTheDocument()
    expect(screen.getByText(/sslmode=require/)).toBeInTheDocument()
  })

  it("renders the configuration the describe reported", () => {
    renderWithClient(<Details />, seed())
    expect(screen.getByText("postgres")).toBeInTheDocument()
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
    expect(screen.getByText("gp3")).toBeInTheDocument()
    expect(screen.getByText("Encrypted — always on")).toBeInTheDocument()
  })

  it("renders no pending changes card when nothing is in flight", () => {
    renderWithClient(<Details />, seed())
    expect(screen.queryByText("Pending changes")).toBeNull()
  })

  it("renders PendingModifiedValues when the backend reports them", () => {
    renderWithClient(
      <Details />,
      seed({
        instances: [
          {
            ...INSTANCE,
            DBInstanceStatus: "modifying",
            PendingModifiedValues: {
              DBInstanceClass: "db.t3.small",
              AllocatedStorage: 40,
            },
          },
        ],
      }),
    )
    expect(screen.getByText("Pending changes")).toBeInTheDocument()
    expect(screen.getByText("db.t3.small")).toBeInTheDocument()
    expect(screen.getByText("40 GiB")).toBeInTheDocument()
  })

  it("renders a backup failure as a warning rather than a blank", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ events: [BACKUP_FAILURE_EVENT, BACKUP_CREATED_EVENT] }),
    )
    const alert = screen.getByRole("alert")
    expect(alert).toHaveTextContent("An automated backup failed")
    expect(alert).toHaveTextContent("volume busy")
  })

  it("dates the last backup from the newest automated snapshot", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ snapshots: AUTOMATED_SNAPSHOTS }),
    )
    expect(screen.queryByRole("alert")).toBeNull()
    expect(screen.getByText("Last backup").parentElement).toHaveTextContent(
      formatDateTime(new Date("2026-08-15T03:02:00Z")),
    )
    expect(screen.getByText("7 days")).toBeInTheDocument()
  })

  it("leaves the last backup blank when only manual snapshots exist", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ snapshots: [SNAPSHOT] }),
    )
    expect(screen.getByText("Last backup").parentElement).toHaveTextContent("—")
  })

  it("reads a zero retention as backups being off", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ instances: [{ ...INSTANCE, BackupRetentionPeriod: 0 }] }),
    )
    expect(screen.getByText("Disabled")).toBeInTheDocument()
  })

  it("shows the empty state on the events tab", () => {
    renderWithClient(<Details tab="settings" />, seed())
    expect(
      screen.getByText("No events in the last 14 days."),
    ).toBeInTheDocument()
  })

  it("lists events newest first", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ events: [BACKUP_CREATED_EVENT, BACKUP_FAILURE_EVENT] }),
    )

    const times = screen
      .getAllByText(/^2026\/08\/\d{2} /u)
      .map((cell) => cell.textContent)

    expect(times[0]).toBe(formatDateTime(new Date("2026-08-16T03:05:00Z")))
  })

  it("enables only the lifecycle actions the status permits", () => {
    renderWithClient(
      <Details />,
      seed({ instances: [{ ...INSTANCE, DBInstanceStatus: "stopped" }] }),
    )
    expect(screen.getByRole("button", { name: "Start" })).toBeEnabled()
    expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Reboot" })).toBeDisabled()
  })

  it("sends StartDBInstance from the heading action", async () => {
    renderWithClient(
      <Details />,
      seed({ instances: [{ ...INSTANCE, DBInstanceStatus: "stopped" }] }),
    )
    fireEvent.click(screen.getByRole("button", { name: "Start" }))
    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })

  it("confirms Stop before sending it", async () => {
    renderWithClient(<Details />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Stop" }))
    expect(mockSend).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole("button", { name: "Stop" }))
    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    expect(mockSend.mock.calls[0]?.[0].constructor.name).toBe(
      "StopDBInstanceCommand",
    )
  })

  it("opens the delete dialog from the Actions menu", async () => {
    renderWithClient(<Details />, seed())
    await chooseAction("Delete")
    expect(screen.getByText(/tears down "orders-db"/u)).toBeInTheDocument()
  })

  it("renders the tags the describe returned", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ tags: [{ Key: "env", Value: "prod" }] }),
    )
    expect(screen.getByDisplayValue("env")).toBeInTheDocument()
    expect(screen.getByDisplayValue("prod")).toBeInTheDocument()
  })

  it("shows the empty state when the instance has no snapshots", () => {
    renderWithClient(<Details tab="settings" />, seed())
    expect(
      screen.getByText("No snapshots of this instance."),
    ).toBeInTheDocument()
  })

  it("lists the instance's snapshots on the backups tab", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ snapshots: [SNAPSHOT] }),
    )
    const row = rowFor("orders-db-snapshot-20260817-1432")
    expect(within(row).getByRole("link", { name: "Restore" })).toHaveAttribute(
      "href",
      "/rds/restore-db-instance-from-db-snapshot/orders-db-snapshot-20260817-1432",
    )
    expect(within(row).getByRole("button", { name: "Delete" })).toBeEnabled()
  })

  // DeleteDBSnapshot refuses the rds: namespace outright, so retention is the
  // only thing that removes an automated backup.
  it("offers no delete for an automated backup", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ snapshots: [AUTOMATED_SNAPSHOT] }),
    )
    const row = rowFor("rds:orders-db-2026-08-17-03-00")
    expect(within(row).getByRole("button", { name: "Delete" })).toBeDisabled()
  })

  it("reads the automated backup's own status rather than inferring one", () => {
    renderWithClient(
      <Details tab="settings" />,
      seed({ automatedBackups: [{ Status: "active" }] }),
    )
    expect(screen.getByText("active")).toBeInTheDocument()
  })

  // The status is the automated backup's own, so nothing the instance describe
  // returns can refresh it.
  it("refreshes the automated backup status after a modify", async () => {
    mockSend.mockImplementation(
      async (command: { constructor: { name: string } }) => {
        if (command.constructor.name === "DescribeDBInstancesCommand") {
          return { DBInstances: [{ ...INSTANCE, BackupRetentionPeriod: 0 }] }
        }

        if (
          command.constructor.name ===
          "DescribeDBInstanceAutomatedBackupsCommand"
        ) {
          return { DBInstanceAutomatedBackups: [] }
        }

        return {}
      },
    )
    renderWithClient(
      <Details tab="settings" />,
      seed({ automatedBackups: [{ Status: "creating" }] }),
    )
    expect(screen.getByText("creating")).toBeInTheDocument()

    await chooseAction("Modify")
    fireEvent.change(screen.getByLabelText("Backup retention (days)"), {
      target: { value: "0" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Save Changes" }))

    await waitFor(() =>
      expect(
        screen.getByText("None — automated backups are off"),
      ).toBeInTheDocument(),
    )
    expect(screen.getByText("Disabled")).toBeInTheDocument()
  })

  it("says automated backups are off when the backend reports none", () => {
    renderWithClient(<Details tab="settings" />, seed())
    expect(
      screen.getByText("None — automated backups are off"),
    ).toBeInTheDocument()
  })

  it("opens the snapshot form for this instance from the Actions menu", async () => {
    renderWithClient(<Details />, seed())
    await chooseAction("Take snapshot")
    expect(mockNavigate).toHaveBeenCalledWith({
      to: "/rds/describe-db-snapshots",
      search: { create: true, instance: "orders-db" },
    })
  })

  // backing-up is reachable only from a settled instance, so the action is
  // held back rather than offered and then refused.
  it("holds the snapshot action back while the instance is transitioning", async () => {
    renderWithClient(
      <Details />,
      seed({ instances: [{ ...INSTANCE, DBInstanceStatus: "modifying" }] }),
    )
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "Actions" }))
    await expect(
      screen.findByRole("menuitem", { name: "Take snapshot" }),
    ).resolves.toHaveAttribute("aria-disabled", "true")
  })

  it("renders no control for a parameter the backend only stores", () => {
    renderWithClient(<Details />, seed())
    expect(screen.queryByText(/Performance Insights/i)).toBeNull()
    expect(screen.queryByText(/Monitoring interval/i)).toBeNull()
    expect(screen.queryByText(/Copy tags to snapshot/i)).toBeNull()
    expect(screen.queryByText(/minor version upgrade/i)).toBeNull()
  })
})
