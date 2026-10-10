import { act, render, renderHook, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { ThemeProvider, useTheme } from "./theme-provider"

function wrapper({ children }: { children: ReactNode }) {
  return <ThemeProvider defaultTheme="light">{children}</ThemeProvider>
}

function setupThemeEnv() {
  localStorage.clear()
  document.documentElement.classList.remove("light", "dark")
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    value: vi.fn().mockReturnValue({ matches: false }),
  })
}

describe("ThemeProvider", () => {
  beforeEach(setupThemeEnv)

  it("renders children", () => {
    render(
      <ThemeProvider>
        <p>Hello</p>
      </ThemeProvider>,
    )
    expect(screen.getByText("Hello")).toBeInTheDocument()
  })

  it("applies light class to documentElement", () => {
    render(
      <ThemeProvider defaultTheme="light">
        <p>Test</p>
      </ThemeProvider>,
    )
    expect(document.documentElement.classList.contains("light")).toBeTruthy()
  })

  it("applies dark class to documentElement", () => {
    render(
      <ThemeProvider defaultTheme="dark">
        <p>Test</p>
      </ThemeProvider>,
    )
    expect(document.documentElement.classList.contains("dark")).toBeTruthy()
  })

  it("uses system theme when set to system", () => {
    Object.defineProperty(window, "matchMedia", {
      writable: true,
      value: vi.fn().mockReturnValue({ matches: true }),
    })
    render(
      <ThemeProvider defaultTheme="system">
        <p>Test</p>
      </ThemeProvider>,
    )
    expect(document.documentElement.classList.contains("dark")).toBeTruthy()
  })

  it("reads stored theme from localStorage", () => {
    localStorage.setItem("ui-theme", "dark")
    render(
      <ThemeProvider>
        <p>Test</p>
      </ThemeProvider>,
    )
    expect(document.documentElement.classList.contains("dark")).toBeTruthy()
  })

  it("ignores invalid stored theme", () => {
    localStorage.setItem("ui-theme", "invalid")
    render(
      <ThemeProvider defaultTheme="light">
        <p>Test</p>
      </ThemeProvider>,
    )
    expect(document.documentElement.classList.contains("light")).toBeTruthy()
  })

  it("uses custom storageKey", () => {
    localStorage.setItem("custom-key", "dark")
    render(
      <ThemeProvider storageKey="custom-key">
        <p>Test</p>
      </ThemeProvider>,
    )
    expect(document.documentElement.classList.contains("dark")).toBeTruthy()
  })
})

describe("useTheme", () => {
  beforeEach(setupThemeEnv)

  it("throws when used outside ThemeProvider", () => {
    expect(() => renderHook(() => useTheme())).toThrow(
      "useTheme must be used within a ThemeProvider",
    )
  })

  it("returns current theme", () => {
    const { result } = renderHook(() => useTheme(), { wrapper })
    expect(result.current.theme).toBe("light")
  })

  it("setTheme updates theme and localStorage", () => {
    const { result } = renderHook(() => useTheme(), { wrapper })

    act(() => {
      result.current.setTheme("dark")
    })

    expect(result.current.theme).toBe("dark")
    expect(localStorage.getItem("ui-theme")).toBe("dark")
  })

  it("removes previous class when theme changes", () => {
    const { result } = renderHook(() => useTheme(), { wrapper })
    expect(document.documentElement.classList.contains("light")).toBeTruthy()

    act(() => {
      result.current.setTheme("dark")
    })

    expect(document.documentElement.classList.contains("dark")).toBeTruthy()
    expect(document.documentElement.classList.contains("light")).toBeFalsy()
  })
})
