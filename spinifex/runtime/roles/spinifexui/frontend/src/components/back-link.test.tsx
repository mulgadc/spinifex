import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { BackLink } from "./back-link"

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    className,
    ...rest
  }: {
    children: React.ReactNode
    className?: string
    to?: string
  }) => (
    <a className={className} href={rest.to}>
      {children}
    </a>
  ),
}))

describe("BackLink", () => {
  it("renders the link text", () => {
    render(<BackLink to="/">Back to list</BackLink>)
    expect(screen.getByText("Back to list")).toBeInTheDocument()
  })

  it("renders the arrow icon", () => {
    const { container } = render(<BackLink to="/">Back</BackLink>)
    const svg = container.querySelector("svg")
    expect(svg).toBeInTheDocument()
  })

  it("passes link props through", () => {
    render(<BackLink to={"/instances" as never}>Back</BackLink>)
    const link = screen.getByRole("link")
    expect(link).toHaveAttribute("href", "/instances")
  })
})
