import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockRuntimeClient: () => ({ send: mockSend }),
}))

import {
  buildConverseInput,
  isWarmingUpError,
  sendConverse,
  useConverse,
} from "./converse"

const USAGE = { inputTokens: 12, outputTokens: 34, totalTokens: 46 }

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

describe("buildConverseInput", () => {
  it("builds the request from messages, system and inferenceConfig", () => {
    const input = buildConverseInput({
      modelId: "meta.llama3-2-1b-instruct-v1:0",
      messages: [{ role: "user", content: [{ text: "hello" }] }],
      system: "Be terse.",
      inferenceConfig: { temperature: 0.5, topP: 0.9, maxTokens: 256 },
    })
    expect(input).toStrictEqual({
      modelId: "meta.llama3-2-1b-instruct-v1:0",
      messages: [{ role: "user", content: [{ text: "hello" }] }],
      system: [{ text: "Be terse." }],
      inferenceConfig: { temperature: 0.5, topP: 0.9, maxTokens: 256 },
    })
  })

  it("omits system when blank and inferenceConfig when unset", () => {
    const input = buildConverseInput({
      modelId: "m1",
      messages: [{ role: "user", content: [{ text: "hi" }] }],
      system: "   ",
    })
    expect(input.system).toBeUndefined()
    expect(input.inferenceConfig).toBeUndefined()
  })

  it("adds guardrailConfig only when both id and version are set", () => {
    const withGuardrail = buildConverseInput({
      modelId: "m1",
      messages: [],
      guardrailIdentifier: "gr-1",
      guardrailVersion: "DRAFT",
    })
    expect(withGuardrail.guardrailConfig).toStrictEqual({
      guardrailIdentifier: "gr-1",
      guardrailVersion: "DRAFT",
    })

    const withoutVersion = buildConverseInput({
      modelId: "m1",
      messages: [],
      guardrailIdentifier: "gr-1",
    })
    expect(withoutVersion.guardrailConfig).toBeUndefined()
  })
})

describe("sendConverse", () => {
  beforeEach(() => {
    mockSend.mockReset()
  })

  it("maps a successful response to the assistant message and usage", async () => {
    const assistantMessage = {
      role: "assistant",
      content: [{ text: "hi there" }],
    }
    mockSend.mockResolvedValue({
      output: { message: assistantMessage },
      usage: USAGE,
      stopReason: "end_turn",
    })

    const outcome = await sendConverse({
      modelId: "m1",
      messages: [{ role: "user", content: [{ text: "hello" }] }],
    })

    expect(outcome.result).toStrictEqual({
      status: "ok",
      message: assistantMessage,
      usage: USAGE,
    })
    expect(outcome.request.modelId).toBe("m1")
  })

  it("yields the warming-up state on ModelNotReadyException instead of throwing", async () => {
    const error = new Error("model not ready")
    error.name = "ModelNotReadyException"
    mockSend.mockRejectedValue(error)

    const outcome = await sendConverse({
      modelId: "m1",
      messages: [{ role: "user", content: [{ text: "hello" }] }],
    })

    expect(outcome.result).toStrictEqual({ status: "warming-up" })
  })

  it("yields the warming-up state on ServiceUnavailableException instead of throwing", async () => {
    const error = new Error("unavailable")
    error.name = "ServiceUnavailableException"
    mockSend.mockRejectedValue(error)

    const outcome = await sendConverse({
      modelId: "m1",
      messages: [{ role: "user", content: [{ text: "hello" }] }],
    })

    expect(outcome.result).toStrictEqual({ status: "warming-up" })
  })

  it("rethrows other errors", async () => {
    const error = new Error("bad request")
    error.name = "ValidationException"
    mockSend.mockRejectedValue(error)

    await expect(
      sendConverse({
        modelId: "m1",
        messages: [{ role: "user", content: [{ text: "hello" }] }],
      }),
    ).rejects.toThrow("bad request")
  })
})

describe("isWarmingUpError", () => {
  it("recognises ModelNotReadyException and ServiceUnavailableException", () => {
    const notReady = new Error("x")
    notReady.name = "ModelNotReadyException"
    const unavailable = new Error("x")
    unavailable.name = "ServiceUnavailableException"
    expect(isWarmingUpError(notReady)).toBeTruthy()
    expect(isWarmingUpError(unavailable)).toBeTruthy()
  })

  it("rejects other errors and non-errors", () => {
    const other = new Error("x")
    other.name = "ValidationException"
    expect(isWarmingUpError(other)).toBeFalsy()
    expect(isWarmingUpError("not an error")).toBeFalsy()
  })
})

describe("useConverse", () => {
  it("sends ConverseCommand via the runtime client", async () => {
    createQueryClient()
    mockSend.mockResolvedValue({
      output: { message: { role: "assistant", content: [{ text: "hi" }] } },
      usage: USAGE,
    })
    const { result } = renderHook(() => useConverse(), { wrapper })

    result.current.mutate({
      modelId: "m1",
      messages: [{ role: "user", content: [{ text: "hello" }] }],
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      modelId: "m1",
      messages: [{ role: "user", content: [{ text: "hello" }] }],
    })
  })
})
