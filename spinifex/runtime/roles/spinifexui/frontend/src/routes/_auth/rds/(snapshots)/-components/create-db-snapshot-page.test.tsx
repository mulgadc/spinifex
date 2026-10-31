import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { CreateDBSnapshotPage } from "./create-db-snapshot-page"

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

function render(dbInstanceIdentifier?: string, qc = seed(), onClose = vi.fn()) {
  return renderWithClient(
    <CreateDBSnapshotPage
      dbInstanceIdentifier={dbInstanceIdentifier}
      onClose={onClose}
    />,
    qc,
  )
}

function snapshotIdentifierField(): HTMLInputElement {
  return screen.getByLabelText("Snapshot identifier")
}

function submit() {
  fireEvent.click(screen.getByRole("button", { name: "Take snapshot" }))
}

describe("CreateDBSnapshotPage opened from an instance", () => {
  it("fixes the instance and suggests a name from it", () => {
    render("orders-db")
    expect(screen.queryByLabelText("DB instance")).toBeNull()
    expect(screen.getByText("orders-db")).toBeInTheDocument()
    expect(snapshotIdentifierField().value).toMatch(
      /^orders-db-snapshot-\d{8}-\d{4}$/,
    )
  })

  it("sends CreateDBSnapshot for that instance and selects the snapshot", async () => {
    const onClose = vi.fn()
    render("orders-db", seed(), onClose)
    submit()
    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBInstanceIdentifier).toBe("orders-db")
    expect(input.DBSnapshotIdentifier).toMatch(/^orders-db-snapshot-/)
    expect(onClose).toHaveBeenCalledWith(input.DBSnapshotIdentifier)
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
    await expect(
      screen.findByText(/may not begin with/),
    ).resolves.toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("surfaces the refusal when the create fails", async () => {
    mockSend.mockRejectedValueOnce(
      new Error("DB instance orders-db is not available"),
    )
    render("orders-db")
    submit()
    await expect(
      screen.findByText(/is not available/),
    ).resolves.toBeInTheDocument()
  })
})

describe("CreateDBSnapshotPage opened from the snapshots list", () => {
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

  it("refuses the submit until an instance is chosen", async () => {
    render()
    submit()
    await expect(
      screen.findByText("Select the DB instance to snapshot."),
    ).resolves.toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("names the snapshot after the instance the user picks", async () => {
    const user = userEvent.setup()
    render()
    await user.click(screen.getByLabelText("DB instance"))
    await user.click(
      screen.getByRole("option", { name: "billing-db (stopped)" }),
    )
    expect(snapshotIdentifierField().value).toMatch(/^billing-db-snapshot-/)
  })

  it("explains the empty picker rather than offering a dead form", () => {
    render(undefined, seed([INSTANCES[2]]))
    expect(
      screen.getByText(/No DB instance is available or stopped/),
    ).toBeInTheDocument()
  })
})
