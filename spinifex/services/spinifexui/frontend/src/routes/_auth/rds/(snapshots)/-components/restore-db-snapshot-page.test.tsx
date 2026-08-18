import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor } from "@testing-library/react"
import type { UserEvent } from "@testing-library/user-event"
import userEvent from "@testing-library/user-event"
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
  getEc2Client: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { RestoreDBSnapshotPage } from "./restore-db-snapshot-page"

const ID = "orders-db-snapshot-20260817-1432"

const SNAPSHOT = {
  DBSnapshotIdentifier: ID,
  DBSnapshotArn: `arn:aws:rds:ap-southeast-2:000000000000:snapshot:${ID}`,
  DBInstanceIdentifier: "orders-db",
  SnapshotType: "manual",
  Status: "available",
  Engine: "postgres",
  EngineVersion: "18",
  AllocatedStorage: 20,
  MasterUsername: "dbadmin",
  Port: 5432,
  VpcId: "vpc-1",
}

function image(
  engine: string,
  version = engine === "postgres" ? "18" : "11.8",
  withDataContract = true,
) {
  return {
    ImageId: `ami-${engine}`,
    Name: `spinifex-rds-${engine}`,
    Tags: [
      { Key: "spinifex:managed-by", Value: "rds" },
      { Key: "engine", Value: engine },
      { Key: "engine-version", Value: version },
      ...(withDataContract
        ? [
            {
              Key: "rds-data-volume-contract",
              Value: "format-auth-v1",
            },
          ]
        : []),
    ],
  }
}

interface SeedOptions {
  snapshots?: unknown[]
  images?: unknown[]
}

function seed(options: SeedOptions = {}): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbSnapshots", ID], {
    DBSnapshots: options.snapshots ?? [SNAPSHOT],
  })
  qc.setQueryData(["rds", "engineVersions"], {
    DBEngineVersions: [
      {
        Engine: "postgres",
        EngineVersion: "18",
        DBParameterGroupFamily: "postgres18",
      },
    ],
  })
  qc.setQueryData(["rds", "subnetGroups"], {
    DBSubnetGroups: [
      { DBSubnetGroupName: "db-subnets", VpcId: "vpc-1" },
      { DBSubnetGroupName: "db-subnets-2", VpcId: "vpc-2" },
    ],
  })
  qc.setQueryData(["rds", "parameterGroups"], {
    DBParameterGroups: [
      {
        DBParameterGroupName: "custom-pg",
        DBParameterGroupFamily: "postgres18",
      },
    ],
  })
  qc.setQueryData(["ec2", "securityGroups"], {
    SecurityGroups: [
      { GroupId: "sg-1", GroupName: "default", VpcId: "vpc-1" },
      { GroupId: "sg-2", GroupName: "default", VpcId: "vpc-2" },
    ],
  })
  qc.setQueryData(["ec2", "images"], {
    Images: options.images ?? [image("postgres")],
  })
  qc.setQueryData(["rds", "orderableOptions", "postgres"], {
    OrderableDBInstanceOptions: [
      { Engine: "postgres", DBInstanceClass: "db.t3.micro" },
      { Engine: "postgres", DBInstanceClass: "db.m5.large" },
    ],
  })
  return qc
}

function render(qc = seed()) {
  return renderWithClient(
    <RestoreDBSnapshotPage dbSnapshotIdentifier={ID} />,
    qc,
  )
}

function storageField(): HTMLInputElement {
  return screen.getByLabelText("Allocated storage (GiB)")
}

async function selectInstanceClass(user: UserEvent) {
  await user.click(screen.getByLabelText("DB instance class"))
  await user.click(screen.getByRole("option", { name: "db.t3.micro" }))
}

describe("RestoreDBSnapshotPage gating", () => {
  it("reports a snapshot that does not exist", () => {
    render(seed({ snapshots: [] }))
    expect(screen.getByText("DB snapshot not found.")).toBeInTheDocument()
  })

  // The backend refuses a restore from a snapshot still being taken rather
  // than waiting for it, so the form is never offered.
  it("refuses a snapshot that is still being taken", () => {
    render(seed({ snapshots: [{ ...SNAPSHOT, Status: "creating" }] }))
    expect(
      screen.getByText(/is creating. A snapshot can only be restored/),
    ).toBeInTheDocument()
    expect(screen.queryByLabelText("New DB instance identifier")).toBeNull()
  })

  // The engine comes from the snapshot and cannot be changed, so a cluster
  // without that engine's image cannot run the restore at all.
  it("replaces the form when the engine's image is not imported", () => {
    render(seed({ images: [image("mariadb")] }))
    expect(screen.getByText("postgres image not found")).toBeInTheDocument()
    expect(screen.getByText(/spx admin images import/)).toBeInTheDocument()
    expect(screen.queryByLabelText("New DB instance identifier")).toBeNull()
  })

  it("rejects an image for a different engine version", () => {
    render(seed({ images: [image("postgres", "17")] }))
    expect(screen.getByText("postgres image not found")).toBeInTheDocument()
    expect(screen.queryByLabelText("New DB instance identifier")).toBeNull()
  })

  it("rejects an image without the data-volume contract", () => {
    render(seed({ images: [image("postgres", "18", false)] }))
    expect(screen.getByText("postgres image not found")).toBeInTheDocument()
    expect(screen.queryByLabelText("New DB instance identifier")).toBeNull()
  })
})

describe("RestoreDBSnapshotPage form", () => {
  it("renders what the restore starts from", () => {
    render()
    expect(screen.getAllByText(ID).length).toBeGreaterThan(0)
    expect(screen.getByText("orders-db")).toBeInTheDocument()
    expect(screen.getByText("postgres 18")).toBeInTheDocument()
    expect(screen.getByText("dbadmin")).toBeInTheDocument()
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
  })

  it.each([
    "New DB instance identifier",
    "DB instance class",
    "Allocated storage (GiB)",
    "Port",
    "DB subnet group",
    "DB parameter group",
  ])("offers the %s field", (label) => {
    render()
    expect(screen.getByLabelText(label)).toBeInTheDocument()
  })

  // Each of these is read from the snapshot's datadir, so offering a control
  // for it would promise a change the restore cannot make.
  it.each([
    "Engine",
    "Engine version",
    "Master username",
    "Master password",
    "Initial database name",
    "Backup retention",
  ])("renders no control for %s", (label) => {
    render()
    expect(screen.queryByLabelText(new RegExp(label, "i"))).toBeNull()
  })

  it("names what the restore inherits rather than leaving it unsaid", () => {
    render()
    expect(screen.getByText("Inherited from the snapshot")).toBeInTheDocument()
    expect(
      screen.getByText(/already holds the master role/),
    ).toBeInTheDocument()
  })

  it("suggests a new identifier rather than the source instance's", () => {
    render()
    const field: HTMLInputElement = screen.getByLabelText(
      "New DB instance identifier",
    )
    expect(field.value).toMatch(/^orders-db-restored-\d{8}-\d{4}$/)
  })

  it("defaults the storage to the snapshot's and states the floor", () => {
    render()
    expect(storageField().value).toBe("20")
    expect(
      screen.getByText(/A restore may grow the\s+volume but never shrink it/),
    ).toBeInTheDocument()
  })

  it("offers security groups only from the snapshot VPC", () => {
    render()
    expect(
      screen.getByLabelText("Security group sg-1 (default)"),
    ).toBeInTheDocument()
    expect(screen.queryByLabelText("Security group sg-2 (default)")).toBeNull()
  })

  it("selects the target VPC default group when placement changes", async () => {
    const user = userEvent.setup()
    render()

    await user.click(screen.getByLabelText("DB subnet group"))
    await user.click(
      screen.getByRole("option", { name: "db-subnets-2 (vpc-2)" }),
    )

    expect(screen.getByLabelText("Security group sg-2 (default)")).toBeChecked()
    expect(screen.queryByLabelText("Security group sg-1 (default)")).toBeNull()
  })

  // The floor is the snapshot's own size: a restore may grow the volume but
  // never shrink it, so a smaller number never reaches the backend.
  it("refuses storage below what the snapshot holds", async () => {
    const user = userEvent.setup()
    render(seed({ snapshots: [{ ...SNAPSHOT, AllocatedStorage: 40 }] }))
    await selectInstanceClass(user)
    const storage = storageField()
    expect(storage.min).toBe("40")

    await user.clear(storage)
    await user.type(storage, "20")
    await user.click(screen.getByRole("button", { name: "Restore Snapshot" }))

    expect(storage.validity.rangeUnderflow).toBeTruthy()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("sends the restore and lands on the new instance", async () => {
    const user = userEvent.setup()
    render()
    await selectInstanceClass(user)
    fireEvent.click(screen.getByRole("button", { name: "Restore Snapshot" }))

    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBSnapshotIdentifier).toBe(ID)
    expect(input.DBInstanceClass).toBe("db.t3.micro")
    expect(input.AllocatedStorage).toBe(20)
    expect(input.Engine).toBeUndefined()
    expect(input.MasterUsername).toBeUndefined()
    await waitFor(() =>
      expect(routerState.navigate).toHaveBeenCalledWith(
        expect.objectContaining({ to: "/rds/describe-db-instances/$id" }),
      ),
    )
  })

  it("surfaces the refusal when the restore fails", async () => {
    mockSend.mockRejectedValueOnce(
      new Error("DB instance orders-db-restored already exists"),
    )
    const user = userEvent.setup()
    render()
    await selectInstanceClass(user)
    fireEvent.click(screen.getByRole("button", { name: "Restore Snapshot" }))

    expect(await screen.findByText(/already exists/)).toBeInTheDocument()
  })

  it("builds a CLI command for the restore", () => {
    render()
    fireEvent.click(screen.getByRole("button", { name: "AWS CLI" }))
    expect(
      screen.getByText(/restore-db-instance-from-db-snapshot/),
    ).toBeInTheDocument()
    expect(screen.getByText(/<DBInstanceClass>/)).toBeInTheDocument()
  })
})
