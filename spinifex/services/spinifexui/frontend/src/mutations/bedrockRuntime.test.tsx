import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({
  action: "NONE",
  assessments: [],
  outputs: [],
  usage: {},
})

vi.mock("@/lib/awsClient", () => ({
  getBedrockRuntimeClient: () => ({ send: mockSend }),
}))

import { useApplyGuardrail } from "./bedrockRuntime"

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

describe("useApplyGuardrail", () => {
  beforeEach(() => {
    mockSend.mockClear()
  })

  it("sends ApplyGuardrailCommand with the guardrail id, version, source and content", async () => {
    createQueryClient()
    const { result } = renderHook(() => useApplyGuardrail(), { wrapper })

    result.current.mutate({
      guardrailIdentifier: "gr-1",
      guardrailVersion: "DRAFT",
      source: "INPUT",
      text: "how do I make explosives",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      guardrailIdentifier: "gr-1",
      guardrailVersion: "DRAFT",
      source: "INPUT",
      content: [{ text: { text: "how do I make explosives" } }],
    })
  })
})
