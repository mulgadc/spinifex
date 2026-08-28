import type {
  DataSourceSummary,
  KnowledgeBase,
} from "@aws-sdk/client-bedrock-agent"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({ ingestionJobSummaries: [] })
const mockNavigate = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockAgentClient: () => ({ send: mockSend }),
  getBedrockAgentRuntimeClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: ReactNode; to?: string }) => (
    <a href={to}>{children}</a>
  ),
  useNavigate: () => mockNavigate,
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

  it("deletes the knowledge base and navigates back on confirm", async () => {
    mockSend.mockResolvedValueOnce({})
    renderSeeded([], KNOWLEDGE_BASE)

    screen.getByRole("button", { name: "Delete" }).click()
    await screen.findByText("Delete Knowledge Base")
    const deleteButtons = screen.getAllByRole("button", { name: "Delete" })
    deleteButtons.at(-1)?.click()

    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    const input = mockSend.mock.calls[0]?.[0].input as {
      knowledgeBaseId: string
    }
    expect(input.knowledgeBaseId).toBe(KB_ID)
    await waitFor(() => {
      expect(mockNavigate).toHaveBeenCalledWith({
        to: "/bedrock/list-knowledge-bases",
      })
    })
  })

  it("adds a data source via the inline form", async () => {
    mockSend.mockResolvedValueOnce({ dataSource: { dataSourceId: "ds-new" } })
    const user = userEvent.setup()
    renderSeeded([], KNOWLEDGE_BASE)

    await user.click(screen.getByRole("button", { name: "Add data source" }))
    await user.type(screen.getByLabelText("Name"), "s3-docs")
    await user.type(
      screen.getByLabelText("S3 bucket ARN"),
      "arn:aws:s3:::docs-bucket",
    )
    await user.click(screen.getByRole("button", { name: "Add data source" }))

    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    const input = mockSend.mock.calls[0]?.[0].input as {
      knowledgeBaseId: string
      name: string
    }
    expect(input.knowledgeBaseId).toBe(KB_ID)
    expect(input.name).toBe("s3-docs")
  })
})
