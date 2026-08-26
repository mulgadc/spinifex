import type { DataSourceSummary } from "@aws-sdk/client-bedrock-agent"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockAgentClient: () => ({ send: mockSend }),
}))

import { DataSourceCard } from "./data-source-card"

const DATA_SOURCE = {
  knowledgeBaseId: "kb-1",
  dataSourceId: "ds-1",
  name: "s3-docs",
  status: "AVAILABLE",
  updatedAt: new Date("2026-01-01T00:00:00Z"),
} satisfies DataSourceSummary

function renderWithClient() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <DataSourceCard dataSource={DATA_SOURCE} knowledgeBaseId="kb-1" />
    </QueryClientProvider>,
  )
}

/* oxlint-disable promise/avoid-new -- a query that never resolves is the only way to freeze the component in its loading state */
async function pendingPromise<T>(): Promise<T> {
  return await new Promise<T>(() => {})
}
/* oxlint-enable promise/avoid-new */

describe("DataSourceCard", () => {
  it("shows a loading state while jobs are fetched", () => {
    mockSend.mockReturnValue(pendingPromise())
    renderWithClient()
    expect(screen.getByText("Loading jobs…")).toBeInTheDocument()
  })

  it("shows an error state when the jobs request fails", async () => {
    mockSend.mockRejectedValue(new Error("boom"))
    renderWithClient()
    await expect(
      screen.findByText("Failed to load ingestion jobs: boom"),
    ).resolves.toBeInTheDocument()
  })

  it("shows an empty state when no jobs have run", async () => {
    mockSend.mockResolvedValue({ ingestionJobSummaries: [] })
    renderWithClient()
    await expect(
      screen.findByText("No ingestion jobs have run yet."),
    ).resolves.toBeInTheDocument()
  })

  it("renders ingestion job rows with status and document counts", async () => {
    mockSend.mockResolvedValue({
      ingestionJobSummaries: [
        {
          knowledgeBaseId: "kb-1",
          dataSourceId: "ds-1",
          ingestionJobId: "job-1",
          status: "COMPLETE",
          startedAt: new Date("2026-01-01T00:00:00Z"),
          updatedAt: new Date("2026-01-01T00:05:00Z"),
          statistics: {
            numberOfDocumentsScanned: 10,
            numberOfNewDocumentsIndexed: 8,
            numberOfModifiedDocumentsIndexed: 1,
            numberOfDocumentsFailed: 1,
          },
        },
      ],
    })
    renderWithClient()
    await expect(screen.findByText("job-1")).resolves.toBeInTheDocument()
    expect(screen.getByText("COMPLETE")).toBeInTheDocument()
    expect(
      screen.getByText("scanned 10 · indexed 9 · failed 1"),
    ).toBeInTheDocument()
  })
})
