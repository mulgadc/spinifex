import { fireEvent, screen, waitFor } from "@testing-library/react"
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
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { DBInstancesListPage } from "./db-instances-list-page"

const AVAILABLE_INSTANCE = {
  DBInstanceIdentifier: "orders-db",
  DBInstanceStatus: "available",
  Engine: "postgres",
  EngineVersion: "18",
  DBInstanceClass: "db.t3.micro",
  AllocatedStorage: 20,
  DeletionProtection: false,
  Endpoint: { Address: "orders-db.rds.internal", Port: 5432 },
}

function seed(instances: unknown[]) {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "dbInstances"], { DBInstances: instances })
  return qc
}

describe("DBInstancesListPage", () => {
  it("renders a DB instance row", () => {
    renderWithClient(<DBInstancesListPage />, seed([AVAILABLE_INSTANCE]))
    expect(screen.getByRole("link", { name: "orders-db" })).toBeInTheDocument()
    expect(screen.getByText("postgres 18")).toBeInTheDocument()
    expect(screen.getByText("db.t3.micro")).toBeInTheDocument()
    expect(screen.getByText("available")).toBeInTheDocument()
    expect(screen.getByText("orders-db.rds.internal")).toBeInTheDocument()
    expect(screen.getByText("20 GiB")).toBeInTheDocument()
  })

  it("shows the empty state with no instances", () => {
    renderWithClient(<DBInstancesListPage />, seed([]))
    expect(screen.getByText("No DB instances found.")).toBeInTheDocument()
  })

  it("enables only the actions an available instance permits", () => {
    renderWithClient(<DBInstancesListPage />, seed([AVAILABLE_INSTANCE]))
    expect(screen.getByRole("button", { name: "Start" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Stop" })).toBeEnabled()
    expect(screen.getByRole("button", { name: "Reboot" })).toBeEnabled()
    expect(screen.getByRole("button", { name: "Delete" })).toBeEnabled()
  })

  it("enables only the actions a stopped instance permits", () => {
    renderWithClient(
      <DBInstancesListPage />,
      seed([{ ...AVAILABLE_INSTANCE, DBInstanceStatus: "stopped" }]),
    )
    expect(screen.getByRole("button", { name: "Start" })).toBeEnabled()
    expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Reboot" })).toBeDisabled()
  })

  it("offers no action while the instance is creating", () => {
    renderWithClient(
      <DBInstancesListPage />,
      seed([{ ...AVAILABLE_INSTANCE, DBInstanceStatus: "creating" }]),
    )
    expect(screen.getByRole("button", { name: "Start" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Reboot" })).toBeDisabled()
  })

  it("sends StopDBInstance when Stop is clicked", async () => {
    renderWithClient(<DBInstancesListPage />, seed([AVAILABLE_INSTANCE]))
    fireEvent.click(screen.getByRole("button", { name: "Stop" }))
    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      DBInstanceIdentifier: "orders-db",
    })
  })

  it("opens the delete dialog for a row", () => {
    renderWithClient(<DBInstancesListPage />, seed([AVAILABLE_INSTANCE]))
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    expect(screen.getByText(/tears down "orders-db"/)).toBeInTheDocument()
  })

  it("navigates to the create page from the heading action", async () => {
    renderWithClient(<DBInstancesListPage />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Create Database" }))
    expect(routerState.navigate).toHaveBeenCalledWith({
      to: "/rds/create-db-instance",
    })
  })
})
