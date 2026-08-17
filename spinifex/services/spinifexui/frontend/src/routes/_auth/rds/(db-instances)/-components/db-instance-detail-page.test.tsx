import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
  getEc2Client: () => ({ send: mockSend }),
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

import { DBInstanceDetailPage } from "./db-instance-detail-page"

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
  EventCategories: ["backup", "creation"],
  Message: "DB snapshot created.",
}

interface SeedOptions {
  instances?: unknown[]
  events?: unknown[]
  tags?: unknown[]
}

function seed(options: SeedOptions = {}): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbInstances", "orders-db"], {
    DBInstances: options.instances ?? [INSTANCE],
  })
  qc.setQueryData(["rds", "events", "orders-db"], {
    Events: options.events ?? [],
  })
  qc.setQueryData(["rds", "tags", ARN], { TagList: options.tags ?? [] })
  qc.setQueryData(["rds", "tags", ""], { TagList: [] })
  qc.setQueryData(["rds", "parameterGroups"], { DBParameterGroups: [] })
  qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: [] })
  qc.setQueryData(["rds", "orderableOptions", "postgres"], {
    OrderableDBInstanceOptions: [{ DBInstanceClass: "db.t3.micro" }],
  })
  return qc
}

function openTab(name: string) {
  fireEvent.click(screen.getByRole("tab", { name }))
}

describe("DBInstanceDetailPage", () => {
  it("renders the identifier and status", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed(),
    )
    expect(screen.getByText("orders-db")).toBeInTheDocument()
    expect(screen.getByText("available")).toBeInTheDocument()
  })

  it("reports a missing instance rather than rendering an empty shell", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ instances: [] }),
    )
    expect(screen.getByText("DB instance not found.")).toBeInTheDocument()
  })

  it("shows the endpoint on the connectivity tab", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed(),
    )
    expect(screen.getByText("orders-db.rds.internal")).toBeInTheDocument()
    expect(screen.getByText("5432")).toBeInTheDocument()
    expect(screen.getByText("No — private VPC address")).toBeInTheDocument()
  })

  it("offers a ready-to-paste connect command for the engine", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed(),
    )
    fireEvent.click(screen.getByRole("button", { name: "AWS CLI" }))
    expect(screen.getByText("psql")).toBeInTheDocument()
    expect(screen.getByText(/sslmode=require/)).toBeInTheDocument()
  })

  it("renders the configuration the describe reported", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed(),
    )
    openTab("Configuration")
    expect(screen.getByText("postgres")).toBeInTheDocument()
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
    expect(screen.getByText("gp3")).toBeInTheDocument()
    expect(screen.getByText("Encrypted — always on")).toBeInTheDocument()
  })

  it("renders no pending changes card when nothing is in flight", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed(),
    )
    openTab("Configuration")
    expect(screen.queryByText("Pending changes")).toBeNull()
  })

  it("renders PendingModifiedValues when the backend reports them", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
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
    openTab("Configuration")
    expect(screen.getByText("Pending changes")).toBeInTheDocument()
    expect(screen.getByText("db.t3.small")).toBeInTheDocument()
    expect(screen.getByText("40 GiB")).toBeInTheDocument()
  })

  it("renders a backup failure as a warning rather than a blank", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ events: [BACKUP_FAILURE_EVENT, BACKUP_CREATED_EVENT] }),
    )
    openTab("Backups")
    const alert = screen.getByRole("alert")
    expect(alert).toHaveTextContent("An automated backup failed")
    expect(alert).toHaveTextContent("volume busy")
  })

  it("dates the last backup from the creation event", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ events: [BACKUP_CREATED_EVENT] }),
    )
    openTab("Backups")
    expect(screen.queryByRole("alert")).toBeNull()
    expect(screen.getByText("2026-08-15T03:02:00.000Z")).toBeInTheDocument()
    expect(screen.getByText("7 days")).toBeInTheDocument()
  })

  it("reads a zero retention as backups being off", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ instances: [{ ...INSTANCE, BackupRetentionPeriod: 0 }] }),
    )
    openTab("Backups")
    expect(screen.getByText("Disabled")).toBeInTheDocument()
  })

  it("shows the empty state on the events tab", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed(),
    )
    openTab("Events")
    expect(
      screen.getByText("No events in the last 14 days."),
    ).toBeInTheDocument()
  })

  it("lists events newest first", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ events: [BACKUP_CREATED_EVENT, BACKUP_FAILURE_EVENT] }),
    )
    openTab("Events")
    const times = screen
      .getAllByText(/^2026-08-\d{2}T/)
      .map((cell) => cell.textContent)
    expect(times[0]).toBe("2026-08-16T03:05:00.000Z")
  })

  it("enables only the lifecycle actions the status permits", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ instances: [{ ...INSTANCE, DBInstanceStatus: "stopped" }] }),
    )
    expect(screen.getByRole("button", { name: "Start" })).toBeEnabled()
    expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Reboot" })).toBeDisabled()
  })

  it("sends StartDBInstance from the heading action", async () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ instances: [{ ...INSTANCE, DBInstanceStatus: "stopped" }] }),
    )
    fireEvent.click(screen.getByRole("button", { name: "Start" }))
    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })

  it("renders the tags the describe returned", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed({ tags: [{ Key: "env", Value: "prod" }] }),
    )
    openTab("Tags")
    expect(screen.getByDisplayValue("env")).toBeInTheDocument()
    expect(screen.getByDisplayValue("prod")).toBeInTheDocument()
  })

  it("renders no control for a parameter the backend only stores", () => {
    renderWithClient(
      <DBInstanceDetailPage dbInstanceIdentifier="orders-db" />,
      seed(),
    )
    openTab("Configuration")
    expect(screen.queryByText(/Performance Insights/i)).toBeNull()
    expect(screen.queryByText(/Monitoring interval/i)).toBeNull()
    expect(screen.queryByText(/Copy tags to snapshot/i)).toBeNull()
    expect(screen.queryByText(/minor version upgrade/i)).toBeNull()
  })
})
