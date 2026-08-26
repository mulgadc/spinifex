import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({ retrievalResults: [] })

vi.mock("@/lib/awsClient", () => ({
  getBedrockAgentRuntimeClient: () => ({ send: mockSend }),
}))

import { useRetrieve } from "./bedrockAgentRuntime"

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createQueryClient() {
  queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  return queryClient
}

describe("useRetrieve", () => {
  beforeEach(() => {
    mockSend.mockClear()
  })

  it("sends RetrieveCommand with the knowledge base id and query text", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRetrieve(), { wrapper })

    result.current.mutate({ knowledgeBaseId: "kb-1", query: "what is ochre" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
      retrievalQuery: { text: "what is ochre" },
    })
  })
})
