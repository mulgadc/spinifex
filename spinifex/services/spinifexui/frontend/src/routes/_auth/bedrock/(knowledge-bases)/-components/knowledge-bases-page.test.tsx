import type { KnowledgeBaseSummary } from "@aws-sdk/client-bedrock-agent"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    children,
    to,
    params,
  }: {
    children: ReactNode
    to?: string
    params?: Record<string, string>
  }) => (
    <a
      href={`${to}${params?.knowledgeBaseId ? `/${params.knowledgeBaseId}` : ""}`}
    >
      {children}
    </a>
  ),
}))

import { knowledgeBasesQueryOptions } from "@/queries/bedrockAgent"

import { KnowledgeBasesPage } from "./knowledge-bases-page"

function renderSeeded(knowledgeBaseSummaries: KnowledgeBaseSummary[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(knowledgeBasesQueryOptions.queryKey, {
    $metadata: {},
    knowledgeBaseSummaries,
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <KnowledgeBasesPage />
    </QueryClientProvider>,
  )
}

const KB = {
  knowledgeBaseId: "kb-1",
  name: "docs-kb",
  status: "ACTIVE",
  updatedAt: new Date("2026-01-01T00:00:00Z"),
} satisfies KnowledgeBaseSummary

describe("KnowledgeBasesPage", () => {
  it("renders knowledge base cards sorted by name", () => {
    renderSeeded([
      { ...KB, knowledgeBaseId: "kb-2", name: "zebra" },
      { ...KB, knowledgeBaseId: "kb-1", name: "alpha" },
    ])
    const links = screen.getAllByRole("link")
    expect(links.map((l) => l.getAttribute("href"))).toStrictEqual([
      "/bedrock/list-knowledge-bases/$knowledgeBaseId/kb-1",
      "/bedrock/list-knowledge-bases/$knowledgeBaseId/kb-2",
    ])
  })

  it("shows empty state when no knowledge bases", () => {
    renderSeeded([])
    expect(screen.getByText("No knowledge bases found.")).toBeInTheDocument()
  })

  it("links each knowledge base to its detail route", () => {
    renderSeeded([KB])
    expect(screen.getByRole("link", { name: /docs-kb/ })).toHaveAttribute(
      "href",
      "/bedrock/list-knowledge-bases/$knowledgeBaseId/kb-1",
    )
  })

  it("shows the knowledge base id and status", () => {
    renderSeeded([KB])
    expect(screen.getByText("kb-1")).toBeInTheDocument()
    expect(screen.getByText("ACTIVE")).toBeInTheDocument()
  })
})
