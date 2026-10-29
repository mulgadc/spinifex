import type { KnowledgeBaseSummary } from "@aws-sdk/client-bedrock-agent"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"

import { KnowledgeBasesPage } from "./knowledge-bases-page"

function renderSeeded(knowledgeBaseSummaries: KnowledgeBaseSummary[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })

  const onSearchChange = vi.fn()

  render(
    <QueryClientProvider client={queryClient}>
      <KnowledgeBasesPage
        knowledgeBases={knowledgeBaseSummaries}
        onSearchChange={onSearchChange}
        search={CLOSED_PANEL}
      />
    </QueryClientProvider>,
  )

  return onSearchChange
}

const KB = {
  knowledgeBaseId: "kb-1",
  name: "docs-kb",
  status: "ACTIVE",
  updatedAt: new Date("2026-01-01T00:00:00Z"),
} satisfies KnowledgeBaseSummary

describe("KnowledgeBasesPage", () => {
  it("lists knowledge bases sorted by name", () => {
    renderSeeded([
      { ...KB, knowledgeBaseId: "kb-2", name: "zebra" },
      { ...KB, knowledgeBaseId: "kb-1", name: "alpha" },
    ])

    const names = screen
      .getAllByRole("link")
      .map((link) => link.textContent)
      .filter((text) => text !== "Create knowledge base")

    expect(names).toStrictEqual(["alpha", "zebra"])
  })

  it("shows empty state when no knowledge bases", () => {
    renderSeeded([])
    expect(screen.getByText("No knowledge bases found.")).toBeInTheDocument()
  })

  it("opens a knowledge base's panel from its row", async () => {
    const onSearchChange = renderSeeded([KB])
    const user = userEvent.setup()

    await user.click(screen.getByText("kb-1"))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "kb-1",
    })
  })

  it("shows the knowledge base id and status", () => {
    renderSeeded([KB])
    expect(screen.getByText("kb-1")).toBeInTheDocument()
    expect(screen.getByText("ACTIVE")).toBeInTheDocument()
  })

  it("links to the create knowledge base route", () => {
    renderSeeded([])
    expect(
      screen.getByRole("link", { name: "Create knowledge base" }),
    ).toHaveAttribute("href", "/bedrock/list-knowledge-bases/create")
  })
})
