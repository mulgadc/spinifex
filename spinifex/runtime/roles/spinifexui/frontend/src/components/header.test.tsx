import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { Header } from "./header"

vi.mock("@/components/theme-switch", () => ({
  ThemeSwitch: () => <div data-testid="theme-switch" />,
}))

vi.mock("@/components/ui/sidebar", () => ({
  SidebarTrigger: ({ className }: { className?: string }) => (
    <button type="button" className={className} data-testid="sidebar-trigger">
      Toggle
    </button>
  ),
}))

const mockUseAdmin = vi.fn()
vi.mock("@/contexts/admin-context", () => ({
  useAdmin: (): unknown => mockUseAdmin(),
}))

describe("Header", () => {
  it("renders sidebar trigger and theme switch", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: false, license: null })
    render(<Header />)
    expect(screen.getByTestId("sidebar-trigger")).toBeInTheDocument()
    expect(screen.getByTestId("theme-switch")).toBeInTheDocument()
  })

  it("shows upgrade link for admin on open-source license", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: true, license: "open-source" })
    render(<Header />)
    const upgradeLink = screen.getByText("Upgrade")
    expect(upgradeLink).toBeInTheDocument()
    expect(upgradeLink.closest("a")).toHaveAttribute(
      "href",
      "https://mulgadc.com/purchase",
    )
  })

  it("does not show upgrade link for non-admin", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: false, license: "open-source" })
    render(<Header />)
    expect(screen.queryByText("Upgrade")).not.toBeInTheDocument()
  })

  it("does not show upgrade link for commercial license", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: true, license: "commercial" })
    render(<Header />)
    expect(screen.queryByText("Upgrade")).not.toBeInTheDocument()
  })
})
