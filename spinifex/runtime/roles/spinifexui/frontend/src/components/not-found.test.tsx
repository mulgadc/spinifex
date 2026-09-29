import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { NotFound } from "./not-found"

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
    <a href={to}>{children}</a>
  ),
}))

describe("NotFound", () => {
  it("renders the 404 heading", () => {
    render(<NotFound />)
    expect(screen.getByText("404")).toBeInTheDocument()
  })

  it("renders 'Page not found' message", () => {
    render(<NotFound />)
    expect(screen.getByText("Page not found")).toBeInTheDocument()
  })

  it("renders a link back home", () => {
    render(<NotFound />)
    const link = screen.getByRole("link")
    expect(link).toHaveAttribute("href", "/")
    expect(screen.getByText("Go back home")).toBeInTheDocument()
  })
})
