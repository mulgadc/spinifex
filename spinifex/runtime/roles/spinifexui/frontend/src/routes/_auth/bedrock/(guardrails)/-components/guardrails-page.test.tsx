import type { GuardrailSummary } from "@aws-sdk/client-bedrock"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"

import { GuardrailsPage } from "./guardrails-page"

function renderSeeded(guardrails: GuardrailSummary[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })

  const onSearchChange = vi.fn()

  render(
    <QueryClientProvider client={queryClient}>
      <GuardrailsPage
        guardrails={guardrails}
        onSearchChange={onSearchChange}
        search={CLOSED_PANEL}
      />
    </QueryClientProvider>,
  )

  return onSearchChange
}

const GUARDRAIL = {
  id: "gr-1",
  arn: "arn:aws:bedrock:us-east-1:000000000000:guardrail/gr-1",
  name: "content-safety",
  status: "READY",
  version: "1",
  createdAt: new Date("2026-01-01T00:00:00Z"),
  updatedAt: new Date("2026-01-01T00:00:00Z"),
} satisfies GuardrailSummary

describe("GuardrailsPage", () => {
  it("renders a create guardrail link", () => {
    renderSeeded([])
    expect(
      screen.getByRole("link", { name: "Create guardrail" }),
    ).toHaveAttribute("href", "/bedrock/list-guardrails/create")
  })

  it("lists guardrails sorted by name", () => {
    renderSeeded([
      { ...GUARDRAIL, id: "gr-2", name: "zebra" },
      { ...GUARDRAIL, id: "gr-1", name: "alpha" },
    ])

    const names = screen
      .getAllByRole("link")
      .map((link) => link.textContent)
      .filter((text) => text !== "Create guardrail")

    expect(names).toStrictEqual(["alpha", "zebra"])
  })

  it("shows empty state when no guardrails", () => {
    renderSeeded([])
    expect(screen.getByText("No guardrails found.")).toBeInTheDocument()
  })

  it("opens a guardrail's panel from its row", async () => {
    const onSearchChange = renderSeeded([GUARDRAIL])
    const user = userEvent.setup()

    await user.click(screen.getByText("gr-1"))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "gr-1",
    })
  })

  it("shows the guardrail id and status", () => {
    renderSeeded([GUARDRAIL])
    expect(screen.getByText("gr-1")).toBeInTheDocument()
    expect(screen.getByText("READY")).toBeInTheDocument()
  })
})
