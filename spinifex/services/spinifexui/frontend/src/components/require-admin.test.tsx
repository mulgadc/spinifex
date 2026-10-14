import { render, screen } from "@testing-library/react"
import { beforeEach, describe, expect, it, vi } from "vitest"

const mockUseAdmin = vi.fn()

vi.mock("@/contexts/admin-context", () => ({
  useAdmin: (): unknown => mockUseAdmin(),
}))

vi.mock("@tanstack/react-router", () => ({
  Navigate: ({ to }: { to: string }) => <div data-testid="navigate">{to}</div>,
}))

import { RequireAdmin } from "./require-admin"

function renderGuard() {
  return render(
    <RequireAdmin>
      <p>admin content</p>
    </RequireAdmin>,
  )
}

describe("RequireAdmin", () => {
  beforeEach(() => {
    mockUseAdmin.mockReset()
  })

  it("renders nothing while admin detection is loading", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: false, loading: true })
    const { container } = renderGuard()
    expect(container).toBeEmptyDOMElement()
  })

  it("renders children for an admin", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: true, loading: false })
    renderGuard()
    expect(screen.getByText("admin content")).toBeInTheDocument()
    expect(screen.queryByTestId("navigate")).not.toBeInTheDocument()
  })

  it("navigates home for a non-admin once detection settles", () => {
    mockUseAdmin.mockReturnValue({ isAdmin: false, loading: false })
    renderGuard()
    expect(screen.getByTestId("navigate")).toHaveTextContent("/")
    expect(screen.queryByText("admin content")).not.toBeInTheDocument()
  })
})
