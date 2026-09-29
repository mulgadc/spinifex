import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { SandboxWelcomeDialog } from "./sandbox-welcome-dialog"

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    className,
    onClick,
    ...rest
  }: {
    children: React.ReactNode
    className?: string
    onClick?: () => void
    to?: string
  }) => (
    <a className={className} href={rest.to} onClick={onClick}>
      {children}
    </a>
  ),
}))

const DISMISSED_KEY = "spinifex:v1:sandbox-welcome"

function stubHost(hostname: string) {
  vi.stubGlobal("location", { hostname })
}

describe("SandboxWelcomeDialog", () => {
  beforeEach(() => {
    localStorage.clear()
  })

  afterEach(() => {
    vi.unstubAllGlobals()
  })

  it("welcomes the user on the sandbox host", () => {
    stubHost("console.spx3.com")
    render(<SandboxWelcomeDialog />)

    expect(screen.getByText("Welcome to the Mulga sandbox")).toBeInTheDocument()
  })

  it("points at the main areas of the console", () => {
    stubHost("console.spx3.com")
    render(<SandboxWelcomeDialog />)

    expect(
      screen.getByRole("link", { name: /Launch an instance/ }),
    ).toHaveAttribute("href", "/ec2/run-instances")
    expect(
      screen.getByRole("link", { name: /Create a bucket/ }),
    ).toHaveAttribute("href", "/s3/ls")
    expect(screen.getByRole("link", { name: /Access keys/ })).toHaveAttribute(
      "href",
      "/iam/list-users",
    )
  })

  // Every other deployment — localhost, a LAN address, an on-prem name —
  // reaches the same console and must be untouched.
  it("stays out of the way everywhere else", () => {
    for (const hostname of [
      "localhost",
      "127.0.0.1",
      "10.0.0.4",
      "console.example.com",
      "spx3.com",
    ]) {
      stubHost(hostname)
      const { unmount } = render(<SandboxWelcomeDialog />)
      expect(screen.queryByText("Welcome to the Mulga sandbox")).toBeNull()
      unmount()
    }
  })

  it("does not return once it has been dismissed", async () => {
    stubHost("console.spx3.com")
    const { unmount } = render(<SandboxWelcomeDialog />)

    await userEvent.click(screen.getByRole("button", { name: "Get started" }))

    await waitFor(() => {
      expect(screen.queryByText("Welcome to the Mulga sandbox")).toBeNull()
    })
    expect(localStorage.getItem(DISMISSED_KEY)).not.toBeNull()

    unmount()
    render(<SandboxWelcomeDialog />)
    expect(screen.queryByText("Welcome to the Mulga sandbox")).toBeNull()
  })

  it("closes when a starting point is followed", async () => {
    stubHost("console.spx3.com")
    render(<SandboxWelcomeDialog />)

    await userEvent.click(
      screen.getByRole("link", { name: /Launch an instance/ }),
    )

    expect(localStorage.getItem(DISMISSED_KEY)).not.toBeNull()
  })

  // Unusable storage means the welcome shows again next visit. It must not
  // take the dashboard down with it.
  it("survives storage being unavailable", async () => {
    stubHost("console.spx3.com")
    vi.stubGlobal("localStorage", {
      getItem: () => {
        throw new Error("SecurityError")
      },
      setItem: () => {
        throw new Error("QuotaExceededError")
      },
    })

    render(<SandboxWelcomeDialog />)
    expect(screen.getByText("Welcome to the Mulga sandbox")).toBeInTheDocument()

    await userEvent.click(screen.getByRole("button", { name: "Get started" }))
    await waitFor(() => {
      expect(screen.queryByText("Welcome to the Mulga sandbox")).toBeNull()
    })
  })
})
