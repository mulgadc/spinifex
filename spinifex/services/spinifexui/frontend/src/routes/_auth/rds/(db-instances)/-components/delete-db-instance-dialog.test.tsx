import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getRdsClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import {
  DeleteDBInstanceDialog,
  defaultFinalSnapshotIdentifier,
} from "./delete-db-instance-dialog"

function render(props: Partial<{ deletionProtection: boolean }> = {}) {
  return renderWithClient(
    <DeleteDBInstanceDialog
      dbInstanceIdentifier="orders-db"
      deletionProtection={props.deletionProtection ?? false}
      onOpenChange={vi.fn()}
      open={true}
    />,
    createTestQueryClient(),
  )
}

describe("defaultFinalSnapshotIdentifier", () => {
  it("stamps the instance name with a UTC timestamp", () => {
    const at = new Date(Date.UTC(2026, 7, 17, 14, 32))
    expect(defaultFinalSnapshotIdentifier("orders-db", at)).toBe(
      "orders-db-final-20260817-1432",
    )
  })

  it("keeps the identifier inside the 63-character limit", () => {
    const at = new Date(Date.UTC(2026, 7, 17, 14, 32))
    const long = `a${"b".repeat(62)}`
    const result = defaultFinalSnapshotIdentifier(long, at)
    expect(result.length).toBeLessThanOrEqual(63)
    expect(result.endsWith("-final-20260817-1432")).toBeTruthy()
  })
})

describe("DeleteDBInstanceDialog", () => {
  it("defaults to taking a final snapshot with a pre-filled name", () => {
    render()
    const checkbox = screen.getByRole("checkbox")
    expect(checkbox).toBeChecked()
    const field = screen.getByLabelText("Final snapshot name")
    expect((field as HTMLInputElement).value).toMatch(/^orders-db-final-/)
  })

  it("explains that a final snapshot pins the data volume", () => {
    render()
    expect(
      screen.getByText(/retains the underlying data volume/),
    ).toBeInTheDocument()
  })

  it("sends a FinalDBSnapshotIdentifier on the default path", async () => {
    render()
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.SkipFinalSnapshot).toBeFalsy()
    expect(input.FinalDBSnapshotIdentifier).toMatch(/^orders-db-final-/)
  })

  it("requires the identifier to be typed before skipping the snapshot", () => {
    render()
    fireEvent.click(screen.getByRole("checkbox"))
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled()

    fireEvent.change(screen.getByLabelText("Type orders-db to confirm"), {
      target: { value: "wrong-name" },
    })
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled()

    fireEvent.change(screen.getByLabelText("Type orders-db to confirm"), {
      target: { value: "orders-db" },
    })
    expect(screen.getByRole("button", { name: "Delete" })).toBeEnabled()
  })

  it("sends SkipFinalSnapshot once the identifier is confirmed", async () => {
    render()
    fireEvent.click(screen.getByRole("checkbox"))
    fireEvent.change(screen.getByLabelText("Type orders-db to confirm"), {
      target: { value: "orders-db" },
    })
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.SkipFinalSnapshot).toBeTruthy()
    expect(input.FinalDBSnapshotIdentifier).toBeUndefined()
  })

  it("blocks the delete outright when deletion protection is on", () => {
    render({ deletionProtection: true })
    expect(
      screen.getByText(/has deletion protection enabled/),
    ).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull()
  })

  it("points at the modify dialog instead of a delete that would fail", () => {
    render({ deletionProtection: true })
    expect(screen.getByText("Open orders-db to modify it")).toBeInTheDocument()
  })

  it("refuses a malformed final snapshot name", () => {
    render()
    fireEvent.change(screen.getByLabelText("Final snapshot name"), {
      target: { value: "Orders--DB-" },
    })
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled()
  })
})
