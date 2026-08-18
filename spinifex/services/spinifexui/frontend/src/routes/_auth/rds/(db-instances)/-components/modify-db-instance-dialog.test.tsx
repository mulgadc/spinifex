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

import { ModifyDBInstanceDialog } from "./modify-db-instance-dialog"

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

function render() {
  return renderWithClient(
    <ModifyDBInstanceDialog
      instance={INSTANCE}
      onOpenChange={vi.fn()}
      open={true}
    />,
    seed(),
  )
}

describe("ModifyDBInstanceDialog", () => {
  it("opens with the instance's current values", () => {
    render()
    expect(screen.getByText("Modify orders-db")).toBeInTheDocument()
    expect(screen.getByLabelText("Allocated storage (GiB)")).toHaveValue(40)
    expect(screen.getByLabelText("Backup retention (days)")).toHaveValue(7)
    expect(screen.getByLabelText("Preferred backup window")).toHaveValue(
      "03:00-03:30",
    )
    expect(screen.getByLabelText("Security group sg-1 (default)")).toBeChecked()
    expect(screen.getByLabelText("Security group sg-2 (db)")).not.toBeChecked()
  })

  it("floors the storage field at the current size", () => {
    render()
    expect(screen.getByLabelText("Allocated storage (GiB)")).toHaveAttribute(
      "min",
      "40",
    )
    expect(screen.getByText(/Currently 40 GiB/)).toBeInTheDocument()
  })

  it("warns that growing storage bounces the instance", () => {
    render()
    fireEvent.change(screen.getByLabelText("Allocated storage (GiB)"), {
      target: { value: "80" },
    })
    expect(
      screen.getByText(/stops and starts the instance/),
    ).toBeInTheDocument()
  })

  it("stays quiet about downtime while nothing has changed", () => {
    render()
    expect(screen.queryByText(/stops and starts the instance/)).toBeNull()
    expect(screen.queryByText(/replaces the VM/)).toBeNull()
  })

  it.each(["Identifier", "Engine version", "Port", "DB subnet group"])(
    "explains that %s is fixed at create",
    (label) => {
      render()
      expect(screen.getByText(label)).toBeInTheDocument()
    },
  )

  it("refuses a shrink rather than sending one", () => {
    render()
    const storage = screen.getByLabelText<HTMLInputElement>(
      "Allocated storage (GiB)",
    )
    fireEvent.change(storage, { target: { value: "20" } })
    fireEvent.click(screen.getByRole("button", { name: "Save Changes" }))

    expect(storage.checkValidity()).toBeFalsy()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("sends the identifier and ApplyImmediately on save", async () => {
    render()
    fireEvent.click(screen.getByLabelText("Apply immediately"))
    fireEvent.click(screen.getByRole("button", { name: "Save Changes" }))

    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBInstanceIdentifier).toBe("orders-db")
    expect(input.ApplyImmediately).toBeTruthy()
    expect(input.MasterUserPassword).toBeUndefined()
  })
})
