import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { SidebarProvider } from "@/components/ui/sidebar"

import { SidebarLayout } from "./sidebar-layout"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return { ...(await mockRouter(importOriginal)), useLocation: () => "/" }
})

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ clear: vi.fn() }),
}))

const mockUseAdmin = vi.fn()

vi.mock("@/hooks/use-admin", () => ({
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

  it("renders each item as a link rather than a button inside a link", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: false })
    mockIsOchreEnabled.mockReturnValue(false)

    renderSidebar()

    const link = screen.getByRole("link", { name: "Instances" })

    expect(link).toHaveAttribute("href", "/ec2/describe-instances")
    expect(link.querySelector("button")).toBeNull()
  })

  it("hides admin-only items from non-admins", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: false })
    mockIsOchreEnabled.mockReturnValue(false)

    renderSidebar()

    expect(screen.getByText("Buckets")).toBeInTheDocument()
    expect(screen.queryByText("Nodes")).not.toBeInTheDocument()
    expect(screen.queryByText("Service Metrics")).not.toBeInTheDocument()
    expect(screen.queryByText("Docs")).not.toBeInTheDocument()
  })
})
