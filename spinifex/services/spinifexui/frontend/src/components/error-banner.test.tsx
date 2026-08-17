import { render, screen } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import { ErrorBanner } from "./error-banner"

describe("ErrorBanner", () => {
  it("renders the message", () => {
    render(<ErrorBanner msg="Something went wrong" />)
    expect(screen.getByText("Something went wrong")).toBeInTheDocument()
  })

  it("renders error.message when error.name is 'Error'", () => {
    const error = new Error("connection refused")
    render(<ErrorBanner error={error} />)
    expect(screen.getByText("connection refused")).toBeInTheDocument()
  })

  it("renders both the name and the message of a custom error type", () => {
    const error = new TypeError("invalid type")
    render(<ErrorBanner error={error} />)
    expect(screen.getByText("TypeError: invalid type")).toBeInTheDocument()
  })

  it("falls back to the name when a custom error carries no message", () => {
    const error = new TypeError("")
    render(<ErrorBanner error={error} />)
    expect(screen.getByText("TypeError")).toBeInTheDocument()
  })

  it("renders both msg and error together", () => {
    const error = new Error("timeout")
    render(<ErrorBanner error={error} msg="Request failed" />)
    expect(screen.getByText("Request failed")).toBeInTheDocument()
    expect(screen.getByText("timeout")).toBeInTheDocument()
  })

  it("renders no text content when no props are provided", () => {
    const { container } = render(<ErrorBanner />)
    expect(container.textContent).toBe("")
  })
})
