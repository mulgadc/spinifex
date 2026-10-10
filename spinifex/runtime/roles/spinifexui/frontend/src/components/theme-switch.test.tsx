import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it } from "vitest"

import { ThemeProvider, useTheme } from "./theme-provider"
import { ThemeSwitch } from "./theme-switch"

function ThemeDisplay() {
  const { theme } = useTheme()
  return <span data-testid="theme">{theme}</span>
}

function renderWithTheme(defaultTheme: "light" | "dark" | "system" = "light") {
  return render(
    <ThemeProvider defaultTheme={defaultTheme}>
      <ThemeSwitch />
      <ThemeDisplay />
    </ThemeProvider>,
  )
}

describe("ThemeSwitch", () => {
  it("renders toggle button", () => {
    renderWithTheme()
    expect(
      screen.getByRole("button", { name: "Toggle theme" }),
    ).toBeInTheDocument()
  })

  it("toggles from light to dark", async () => {
    const user = userEvent.setup()
    renderWithTheme("light")

    await user.click(screen.getByRole("button", { name: "Toggle theme" }))
    expect(screen.getByTestId("theme")).toHaveTextContent("dark")
  })

  it("toggles from dark to light", async () => {
    const user = userEvent.setup()
    renderWithTheme("dark")

    await user.click(screen.getByRole("button", { name: "Toggle theme" }))
    expect(screen.getByTestId("theme")).toHaveTextContent("light")
  })

  it("toggles from system to dark", async () => {
    const user = userEvent.setup()
    renderWithTheme("system")

    await user.click(screen.getByRole("button", { name: "Toggle theme" }))
    expect(screen.getByTestId("theme")).toHaveTextContent("dark")
  })
})
