import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { BackLink } from "./back-link"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

describe("BackLink", () => {
  it("renders the link text", () => {
    render(<BackLink to="/">Back to list</BackLink>)
    expect(screen.getByText("Back to list")).toBeInTheDocument()
  })

  it("passes link props through", () => {
    render(<BackLink to={"/instances" as never}>Back</BackLink>)
    const link = screen.getByRole("link")
    expect(link).toHaveAttribute("href", "/instances")
  })
})
