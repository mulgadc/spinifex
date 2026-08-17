import { fireEvent, screen, waitFor } from "@testing-library/react"
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

import { CreateDBParameterGroupPage } from "./create-db-parameter-group-page"

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["rds", "engineVersions"], {
    DBEngineVersions: [
      {
        Engine: "postgres",
        EngineVersion: "18",
        DBParameterGroupFamily: "postgres18",
        DBEngineVersionDescription: "PostgreSQL 18",
      },
      {
        Engine: "mariadb",
        EngineVersion: "11.8",
        DBParameterGroupFamily: "mariadb11.8",
        DBEngineVersionDescription: "MariaDB 11.8",
      },
    ],
  })
  return qc
}

describe("CreateDBParameterGroupPage", () => {
  // The families come from the engine catalog, so a version pin bump moves the
  // picker without the console being edited.
  it("reads the families from the engine catalog", async () => {
    const user = userEvent.setup()
    renderWithClient(<CreateDBParameterGroupPage />, seed())

    await user.click(screen.getByLabelText("Family"))

    expect(
      screen.getByRole("option", { name: "postgres18 — PostgreSQL 18" }),
    ).toBeInTheDocument()
    expect(
      screen.getByRole("option", { name: "mariadb11.8 — MariaDB 11.8" }),
    ).toBeInTheDocument()
  })

  it("sends CreateDBParameterGroup with the family", async () => {
    renderWithClient(<CreateDBParameterGroupPage />, seed())

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "orders-pg" },
    })
    fireEvent.change(screen.getByLabelText("Description"), {
      target: { value: "Tuned settings" },
    })
    fireEvent.click(
      screen.getByRole("button", { name: "Create parameter group" }),
    )

    await waitFor(() => expect(mockSend).toHaveBeenCalled())
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.DBParameterGroupName).toBe("orders-pg")
    expect(input.DBParameterGroupFamily).toBe("postgres18")
    expect(input.Description).toBe("Tuned settings")
  })

  it("refuses the reserved default prefix before the round trip", async () => {
    renderWithClient(<CreateDBParameterGroupPage />, seed())

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "default.postgres18" },
    })
    fireEvent.change(screen.getByLabelText("Description"), {
      target: { value: "Tuned settings" },
    })
    fireEvent.click(
      screen.getByRole("button", { name: "Create parameter group" }),
    )

    expect(
      await screen.findByText(/may not begin with "default\."/),
    ).toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })

  it("requires a description, as the backend does", async () => {
    renderWithClient(<CreateDBParameterGroupPage />, seed())

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "orders-pg" },
    })
    fireEvent.click(
      screen.getByRole("button", { name: "Create parameter group" }),
    )

    expect(
      await screen.findByText("Description is required"),
    ).toBeInTheDocument()
    expect(mockSend).not.toHaveBeenCalled()
  })
})
