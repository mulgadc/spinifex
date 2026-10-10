import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { SidebarProvider } from "@/components/ui/sidebar"

import { SidebarLayout } from "./sidebar-layout"

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
    <a href={to}>{children}</a>
  ),
  useLocation: () => "/",
  useNavigate: () => vi.fn(),
}))

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ clear: vi.fn() }),
}))

const mockUseAdmin = vi.fn()
vi.mock("@/contexts/admin-context", () => ({
  useAdmin: (): unknown => mockUseAdmin(),
}))

const mockIsOchreEnabled = vi.fn()
vi.mock("@/lib/cluster-config", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/cluster-config")>()
  return {
    ...actual,
    isOchreEnabled: (): unknown => mockIsOchreEnabled(),
  }
})

function renderSidebar() {
  return render(
    <SidebarProvider>
      <SidebarLayout />
    </SidebarProvider>,
  )
}

describe("SidebarLayout", () => {
  it("hides the Ochre nav group when the flag is off", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: true })
    mockIsOchreEnabled.mockReturnValue(false)

    renderSidebar()

    expect(screen.queryByText("Ochre")).not.toBeInTheDocument()
    expect(screen.queryByText("Knowledge Bases")).not.toBeInTheDocument()
  })

  it("shows the Ochre nav group when the flag is on", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: true })
    mockIsOchreEnabled.mockReturnValue(true)

    renderSidebar()

    expect(screen.getByText("Ochre")).toBeInTheDocument()
    expect(screen.getByText("Knowledge Bases")).toBeInTheDocument()
    expect(screen.getByText("Guardrails")).toBeInTheDocument()
    expect(screen.getByText("Playground")).toBeInTheDocument()
  })
})
