import type {
  DataSourceSummary,
  KnowledgeBase,
} from "@aws-sdk/client-bedrock-agent"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({ ingestionJobSummaries: [] })

vi.mock("@/lib/awsClient", () => ({
  getBedrockAgentClient: () => ({ send: mockSend }),
  getBedrockAgentRuntimeClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: ReactNode; to?: string }) => (
    <a href={to}>{children}</a>
  ),
}))

import {
  dataSourcesQueryOptions,
  knowledgeBaseQueryOptions,
} from "@/queries/bedrockAgent"

import { KnowledgeBaseDetailPage } from "./knowledge-base-detail-page"

const KB_ID = "kb-1"

const KNOWLEDGE_BASE = {
  knowledgeBaseId: KB_ID,
  name: "docs-kb",
  knowledgeBaseArn: "arn:aws:bedrock:local::knowledge-base/kb-1",
  description: "Docs for the platform",
  roleArn: "arn:aws:iam::local:role/kb-role",
  knowledgeBaseConfiguration: {
    type: "VECTOR",
    vectorKnowledgeBaseConfiguration: {
      embeddingModelArn: "arn:aws:bedrock:local::foundation-model/embed",
    },
  },
  status: "ACTIVE",
  createdAt: new Date("2026-01-01T00:00:00Z"),
  updatedAt: new Date("2026-01-02T00:00:00Z"),
} satisfies KnowledgeBase

function renderSeeded(
  dataSourceSummaries: DataSourceSummary[],
  knowledgeBase: KnowledgeBase | undefined,
) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(knowledgeBaseQueryOptions(KB_ID).queryKey, {
    $metadata: {},
    knowledgeBase,
  })
  queryClient.setQueryData(dataSourcesQueryOptions(KB_ID).queryKey, {
    $metadata: {},
    dataSourceSummaries,
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <KnowledgeBaseDetailPage knowledgeBaseId={KB_ID} />
    </QueryClientProvider>,
  )
}

describe("KnowledgeBaseDetailPage", () => {
  it("renders knowledge base details", () => {
    renderSeeded([], KNOWLEDGE_BASE)
    expect(screen.getByText("docs-kb")).toBeInTheDocument()
    expect(
      screen.getByText("arn:aws:bedrock:local::knowledge-base/kb-1"),
    ).toBeInTheDocument()
    expect(screen.getByText("Docs for the platform")).toBeInTheDocument()
  })

  it("shows the empty state when there are no data sources", () => {
    renderSeeded([], KNOWLEDGE_BASE)
    expect(
      screen.getByText("No data sources configured for this knowledge base."),
    ).toBeInTheDocument()
  })

  it("renders a card per data source", () => {
    renderSeeded(
      [
        {
          knowledgeBaseId: KB_ID,
          dataSourceId: "ds-1",
          name: "s3-docs",
          status: "AVAILABLE",
          updatedAt: new Date("2026-01-01T00:00:00Z"),
        },
      ],
      KNOWLEDGE_BASE,
    )
    expect(screen.getByText("s3-docs")).toBeInTheDocument()
    expect(screen.getByText("ds-1")).toBeInTheDocument()
  })

  it("shows a not-found message when the knowledge base is missing", () => {
    renderSeeded([], undefined)
    expect(screen.getByText("Knowledge base not found.")).toBeInTheDocument()
  })

  it("renders the retrieve tester", () => {
    renderSeeded([], KNOWLEDGE_BASE)
    expect(screen.getByText("Retrieve tester")).toBeInTheDocument()
  })
})
