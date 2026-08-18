import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

import { CreateDBSnapshotDialog } from "./create-db-snapshot-dialog"

const INSTANCES = [
  { DBInstanceIdentifier: "orders-db", DBInstanceStatus: "available" },
  { DBInstanceIdentifier: "billing-db", DBInstanceStatus: "stopped" },
  { DBInstanceIdentifier: "reports-db", DBInstanceStatus: "creating" },
]

function seed(instances: unknown[] = INSTANCES): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbInstances"], { DBInstances: instances })
  return qc
}

function render(dbInstanceIdentifier?: string, qc = seed()) {
  return renderWithClient(
    <CreateDBSnapshotDialog
      dbInstanceIdentifier={dbInstanceIdentifier}
      onOpenChange={vi.fn()}
      open={true}
    />,
    qc,
  )
}

function snapshotIdentifierField(): HTMLInputElement {
  return screen.getByLabelText("Snapshot identifier")
}

function submit() {
  fireEvent.click(screen.getByRole("button", { name: "Take Snapshot" }))
}

describe("CreateDBSnapshotDialog opened from an instance", () => {
  it("fixes the instance and suggests a name from it", () => {
    render("orders-db")
    expect(screen.queryByLabelText("DB instance")).toBeNull()
    expect(screen.getByText("orders-db")).toBeInTheDocument()
    expect(snapshotIdentifierField().value).toMatch(
      /^orders-db-snapshot-\d{8}-\d{4}$/,
    )
  })

  it("sends CreateDBSnapshot for that instance", async () => {
    render("orders-db")
    submit()
    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBInstanceIdentifier).toBe("orders-db")
    expect(input.DBSnapshotIdentifier).toMatch(/^orders-db-snapshot-/)
  })

  it("says the instance reads as backing-up while the snapshot is taken", () => {
    render("orders-db")
    expect(screen.getByText(/reads as backing-up/)).toBeInTheDocument()
  })

  // The name is the one field the backend refuses outright, so the refusal is
  // rendered rather than sent.
  it("refuses the reserved rds: namespace before sending", async () => {
    const user = userEvent.setup()
    render("orders-db")
    const field = screen.getByLabelText("Snapshot identifier")
    await user.clear(field)
    await user.type(field, "rds:orders-db")
    submit()
    expect(await screen.findByText(/may not begin with/)).toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("surfaces the refusal when the create fails", async () => {
    mockSend.mockRejectedValueOnce(
      new Error("DB instance orders-db is not available"),
    )
    render("orders-db")
    submit()
    expect(await screen.findByText(/is not available/)).toBeInTheDocument()
  })
})

describe("CreateDBSnapshotDialog opened from the snapshots list", () => {
  it("offers only the instances a snapshot can be taken from", async () => {
    const user = userEvent.setup()
    render()
    await user.click(screen.getByLabelText("DB instance"))
    expect(
      screen.getByRole("option", { name: "orders-db (available)" }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("option", { name: "billing-db (stopped)" }),
    ).toBeInTheDocument()
    expect(screen.queryByRole("option", { name: /reports-db/ })).toBeNull()
  })

  it("holds the submit back until an instance is chosen", () => {
    render()
    expect(screen.getByRole("button", { name: "Take Snapshot" })).toBeDisabled()
  })

  it("names the snapshot after the instance the user picks", async () => {
    const user = userEvent.setup()
    render()
    await user.click(screen.getByLabelText("DB instance"))
    await user.click(
      screen.getByRole("option", { name: "billing-db (stopped)" }),
    )
    expect(snapshotIdentifierField().value).toMatch(/^billing-db-snapshot-/)
    expect(screen.getByRole("button", { name: "Take Snapshot" })).toBeEnabled()
  })

  it("explains the empty picker rather than offering a dead form", () => {
    render(undefined, seed([INSTANCES[2]]))
    expect(
      screen.getByText(/No DB instance is available or stopped/),
    ).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Take Snapshot" })).toBeDisabled()
  })
})
