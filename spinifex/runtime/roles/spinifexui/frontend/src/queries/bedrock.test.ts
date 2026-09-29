import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getBedrockClient: () => ({ send: mockSend }),
}))

import { callQueryFn } from "@/test/query"

import {
  guardrailQueryOptions,
  guardrailsQueryOptions,
  guardrailVersionsQueryOptions,
} from "./bedrock"

describe("query keys", () => {
  it("guardrails list key", () => {
    expect(guardrailsQueryOptions.queryKey).toStrictEqual([
      "bedrock",
      "guardrails",
    ])
  })

  it("guardrail key includes id", () => {
    expect(guardrailQueryOptions("gr-1").queryKey).toStrictEqual([
      "bedrock",
      "guardrails",
      "gr-1",
    ])
  })

  it("guardrail versions key includes id", () => {
    expect(guardrailVersionsQueryOptions("gr-1").queryKey).toStrictEqual([
      "bedrock",
      "guardrails",
      "gr-1",
      "versions",
    ])
  })
})

describe("query functions", () => {
  beforeEach(() => {
    mockSend.mockClear()
  })

  it("lists guardrails", async () => {
    mockSend.mockResolvedValueOnce({ guardrails: [] })
    await callQueryFn(guardrailsQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("gets a guardrail by id", async () => {
    mockSend.mockResolvedValueOnce({ guardrailId: "gr-1" })
    await callQueryFn(guardrailQueryOptions("gr-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      guardrailIdentifier: "gr-1",
    })
  })

  it("lists a guardrail's versions", async () => {
    mockSend.mockResolvedValueOnce({ guardrails: [] })
    await callQueryFn(guardrailVersionsQueryOptions("gr-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      guardrailIdentifier: "gr-1",
    })
  })
})
