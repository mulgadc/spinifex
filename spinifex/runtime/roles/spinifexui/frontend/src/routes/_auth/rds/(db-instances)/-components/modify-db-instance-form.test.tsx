import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { ModifyDBInstanceForm } from "./modify-db-instance-form"

const INSTANCE = {
  DBInstanceIdentifier: "orders-db",
  DBInstanceStatus: "available",
  Engine: "postgres",
  EngineVersion: "18",
  DBInstanceClass: "db.t3.micro",
  AllocatedStorage: 40,
  DeletionProtection: false,
  BackupRetentionPeriod: 7,
  PreferredBackupWindow: "03:00-03:30",
  PreferredMaintenanceWindow: "sun:04:00-sun:04:30",
  VpcSecurityGroups: [{ VpcSecurityGroupId: "sg-1" }],
  DBParameterGroups: [{ DBParameterGroupName: "default.postgres18" }],
}

function seed(): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "engineVersions"], {
    DBEngineVersions: [
      { Engine: "postgres", DBParameterGroupFamily: "postgres18" },
      { Engine: "mariadb", DBParameterGroupFamily: "mariadb11.8" },
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
      { GroupId: "sg-2", GroupName: "db", VpcId: "vpc-1" },
    ],
  })
  qc.setQueryData(["rds", "orderableOptions", "postgres"], {
    OrderableDBInstanceOptions: [
      { DBInstanceClass: "db.t3.micro" },
      { DBInstanceClass: "db.t3.small" },
    ],
  })

  return qc
}

function render(onClose = vi.fn()) {
  renderWithClient(
    <ModifyDBInstanceForm instance={INSTANCE} onClose={onClose} />,
    seed(),
  )

  return within(screen.getByRole("region", { name: "Modify orders-db" }))
}

describe("ModifyDBInstanceForm", () => {
  it("opens with the instance's current values", () => {
    const panel = render()
    expect(panel.getByLabelText("Allocated storage (GiB)")).toHaveValue(40)
    expect(panel.getByLabelText("Backup retention (days)")).toHaveValue(7)
    expect(panel.getByLabelText("Backup window start")).toHaveTextContent(
      "03:00",
    )
    expect(panel.getByLabelText("Backup window length")).toHaveTextContent(
      "30 min",
    )
    expect(panel.getByLabelText("Maintenance window day")).toHaveTextContent(
      "Sunday",
    )
    expect(panel.getByLabelText("Maintenance window start")).toHaveTextContent(
      "04:00",
    )
    expect(panel.getByLabelText("Security group sg-1 (default)")).toBeChecked()
    expect(panel.getByLabelText("Security group sg-2 (db)")).not.toBeChecked()
  })

  it("floors the storage field at the current size", () => {
    const panel = render()
    expect(panel.getByLabelText("Allocated storage (GiB)")).toHaveAttribute(
      "min",
      "40",
    )
    expect(panel.getByText(/Currently 40 GiB/)).toBeInTheDocument()
  })

  it("warns that growing storage bounces the instance", () => {
    const panel = render()
    fireEvent.change(panel.getByLabelText("Allocated storage (GiB)"), {
      target: { value: "80" },
    })
    expect(panel.getByText(/stops and starts the instance/)).toBeInTheDocument()
  })

  it("stays quiet about downtime while nothing has changed", () => {
    const panel = render()
    expect(panel.queryByText(/stops and starts the instance/)).toBeNull()
    expect(panel.queryByText(/replaces the VM/)).toBeNull()
  })

  it.each(["Identifier", "Engine version", "Port", "DB subnet group"])(
    "explains that %s is fixed at create",
    (label) => {
      const panel = render()
      expect(panel.getByText(label)).toBeInTheDocument()
    },
  )

  it("refuses a shrink rather than sending one", () => {
    const panel = render()

    const storage = panel.getByLabelText<HTMLInputElement>(
      "Allocated storage (GiB)",
    )

    fireEvent.change(storage, { target: { value: "20" } })
    fireEvent.click(panel.getByRole("button", { name: "Save Changes" }))

    expect(storage.checkValidity()).toBeFalsy()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("sends the identifier and ApplyImmediately on save", async () => {
    const panel = render()
    fireEvent.click(panel.getByLabelText("Apply immediately"))
    fireEvent.click(panel.getByRole("button", { name: "Save Changes" }))

    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBInstanceIdentifier).toBe("orders-db")
    expect(input.ApplyImmediately).toBeTruthy()
    expect(input.MasterUserPassword).toBeUndefined()
  })

  it("builds a CLI command that names the password by variable only", () => {
    const panel = render()
    fireEvent.change(panel.getByLabelText("Reset master password"), {
      target: { value: "hunter2-secret" },
    })
    fireEvent.click(panel.getByRole("button", { name: "AWS CLI" }))
    expect(panel.getByText(/--master-user-password/)).toBeInTheDocument()
    expect(panel.queryByText(/hunter2-secret/)).toBeNull()
    expect(panel.getByText(/--no-apply-immediately/)).toBeInTheDocument()
  })

  it("shows a refused save in the form and keeps it open", async () => {
    mockSend.mockRejectedValueOnce(
      Object.assign(new Error("DB instance orders-db is not available"), {
        name: "InvalidDBInstanceState",
      }),
    )
    const onClose = vi.fn()
    const panel = render(onClose)
    fireEvent.click(panel.getByRole("button", { name: "Save Changes" }))

    await expect(
      panel.findByText(
        "InvalidDBInstanceState: DB instance orders-db is not available",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(panel.getByRole("button", { name: "Save Changes" })).toBeEnabled()
  })

  it("sends a window moved with the pickers", async () => {
    const user = userEvent.setup()
    const panel = render()
    mockSend.mockClear()
    await user.click(panel.getByLabelText("Backup window length"))
    await user.click(screen.getByRole("option", { name: "1 hour" }))
    await user.click(panel.getByRole("button", { name: "Save Changes" }))

    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    expect(mockSend.mock.calls[0]?.[0].input.PreferredBackupWindow).toBe(
      "03:00-04:00",
    )
  })

  it("refuses a maintenance window over the backup window", async () => {
    const user = userEvent.setup()
    const panel = render()
    mockSend.mockClear()
    await user.click(panel.getByLabelText("Maintenance window start"))
    await user.click(screen.getByRole("option", { name: "03:00" }))
    await user.click(panel.getByRole("button", { name: "Save Changes" }))

    await expect(
      panel.findByText(
        "The maintenance window overlaps the backup window at that time of day",
      ),
    ).resolves.toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })
})
