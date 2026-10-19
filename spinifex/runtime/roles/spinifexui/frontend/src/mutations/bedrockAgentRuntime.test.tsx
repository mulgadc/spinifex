import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() =>
  vi.fn().mockResolvedValue({ retrievalResults: [] }),
)

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { useRetrieve } from "./bedrockAgentRuntime"

describe("useRetrieve", () => {
  it("sends RetrieveCommand with the knowledge base id and query text", async () => {
    const { result } = renderMutationHook(() => useRetrieve())

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
