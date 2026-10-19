import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() =>
  vi.fn().mockResolvedValue({
    action: "NONE",
    assessments: [],
    outputs: [],
    usage: {},
  }),
)

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { useApplyGuardrail } from "./bedrockRuntime"

describe("useApplyGuardrail", () => {
  it("sends ApplyGuardrailCommand with the guardrail id, version, source and content", async () => {
    const { result } = renderMutationHook(() => useApplyGuardrail())

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
