import type { RetrieveResponse } from "@aws-sdk/client-bedrock-agent-runtime"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { fireEvent, render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockAgentRuntimeClient: () => ({ send: mockSend }),
}))

import { RetrieveTester } from "./retrieve-tester"

function renderTester() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <RetrieveTester knowledgeBaseId="kb-1" />
    </QueryClientProvider>,
  )
}

function submitQuery(text: string) {
  fireEvent.change(screen.getByLabelText("Query"), {
    target: { value: text },
  })
  fireEvent.click(screen.getByRole("button", { name: "Retrieve" }))
}

describe("RetrieveTester", () => {
  it("disables submit until a query is entered", () => {
    renderTester()
    expect(screen.getByRole("button", { name: "Retrieve" })).toBeDisabled()
    fireEvent.change(screen.getByLabelText("Query"), {
      target: { value: "what is ochre" },
    })
    expect(screen.getByRole("button", { name: "Retrieve" })).toBeEnabled()
  })

  it("sends the query and renders returned chunks with score and source", async () => {
    mockSend.mockResolvedValue({
      retrievalResults: [
        {
          content: { text: "Ochre is the Bedrock-compatible service." },
          location: { type: "S3", s3Location: { uri: "s3://kb/doc1.md" } },
          score: 0.9123,
        },
        {
          content: { text: "It self-hosts open weight models." },
          location: { type: "S3", s3Location: { uri: "s3://kb/doc2.md" } },
          score: 0.81,
        },
      ],
    } satisfies RetrieveResponse)

    renderTester()
    submitQuery("what is ochre")

    await expect(
      screen.findByText("Ochre is the Bedrock-compatible service."),
    ).resolves.toBeInTheDocument()
    expect(
      screen.getByText("It self-hosts open weight models."),
    ).toBeInTheDocument()
    expect(screen.getByText("s3://kb/doc1.md")).toBeInTheDocument()
    expect(screen.getByText("score 0.9123")).toBeInTheDocument()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
      retrievalQuery: { text: "what is ochre" },
    })
  })

  it("shows the empty state when no chunks match", async () => {
    mockSend.mockResolvedValue({ retrievalResults: [] })
    renderTester()
    submitQuery("no matches for this")
    await expect(
      screen.findByText("No chunks matched this query."),
    ).resolves.toBeInTheDocument()
  })

  it("shows an error when the retrieve call fails", async () => {
    mockSend.mockRejectedValue(new Error("kb unavailable"))
    renderTester()
    submitQuery("what is ochre")
    await expect(
      screen.findByText("kb unavailable"),
    ).resolves.toBeInTheDocument()
  })
})
