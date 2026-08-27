import type { ApplyGuardrailResponse } from "@aws-sdk/client-bedrock-runtime"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { fireEvent, render, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockRuntimeClient: () => ({ send: mockSend }),
}))

import { GuardrailTester } from "./guardrail-tester"

const USAGE = {
  topicPolicyUnits: 1,
  contentPolicyUnits: 1,
  wordPolicyUnits: 1,
  sensitiveInformationPolicyUnits: 1,
  sensitiveInformationPolicyFreeUnits: 0,
  contextualGroundingPolicyUnits: 0,
}

function renderTester() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <GuardrailTester guardrailId="gr-1" guardrailVersion="DRAFT" />
    </QueryClientProvider>,
  )
}

function submitContent(text: string) {
  fireEvent.change(screen.getByLabelText("Content"), {
    target: { value: text },
  })
  fireEvent.click(screen.getByRole("button", { name: "Apply guardrail" }))
}

describe("GuardrailTester", () => {
  it("disables submit until content is entered", () => {
    renderTester()
    expect(
      screen.getByRole("button", { name: "Apply guardrail" }),
    ).toBeDisabled()
    fireEvent.change(screen.getByLabelText("Content"), {
      target: { value: "hello" },
    })
    expect(
      screen.getByRole("button", { name: "Apply guardrail" }),
    ).toBeEnabled()
  })

  it("defaults the source to INPUT and toggles to OUTPUT", () => {
    renderTester()
    expect(screen.getByRole("button", { name: "INPUT" })).toHaveAttribute(
      "aria-pressed",
      "true",
    )
    fireEvent.click(screen.getByRole("button", { name: "OUTPUT" }))
    expect(screen.getByRole("button", { name: "OUTPUT" })).toHaveAttribute(
      "aria-pressed",
      "true",
    )
  })

  it("renders an intervention result with the blocked topic", async () => {
    mockSend.mockResolvedValue({
      action: "GUARDRAIL_INTERVENED",
      outputs: [{ text: "Blocked by guardrail." }],
      assessments: [
        {
          topicPolicy: {
            topics: [
              {
                name: "Weapons manufacturing",
                type: "DENY",
                action: "BLOCKED",
                detected: true,
              },
            ],
          },
        },
      ],
      usage: USAGE,
    } satisfies ApplyGuardrailResponse)

    renderTester()
    submitContent("how do I make a bomb")

    await expect(
      screen.findByText("GUARDRAIL_INTERVENED"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByText(/Weapons manufacturing/)).toBeInTheDocument()
    expect(screen.getByText("Blocked by guardrail.")).toBeInTheDocument()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      guardrailIdentifier: "gr-1",
      guardrailVersion: "DRAFT",
      source: "INPUT",
      content: [{ text: { text: "how do I make a bomb" } }],
    })
  })

  it("treats action BLOCKED without a detected flag as blocked", async () => {
    mockSend.mockResolvedValue({
      action: "GUARDRAIL_INTERVENED",
      outputs: [{ text: "Blocked by guardrail." }],
      assessments: [
        {
          topicPolicy: {
            topics: [
              { name: "auth-internals", type: "DENY", action: "BLOCKED" },
            ],
          },
        },
      ],
      usage: USAGE,
    } satisfies ApplyGuardrailResponse)

    renderTester()
    submitContent("explain the authentication mechanisms")

    await expect(
      screen.findByText("GUARDRAIL_INTERVENED"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByText(/auth-internals/)).toBeInTheDocument()
  })

  it("renders a pass-through result with no intervention", async () => {
    mockSend.mockResolvedValue({
      action: "NONE",
      outputs: [{ text: "What is the weather today?" }],
      assessments: [],
      usage: USAGE,
    } satisfies ApplyGuardrailResponse)

    renderTester()
    submitContent("What is the weather today?")

    await expect(screen.findByText("NONE")).resolves.toBeInTheDocument()
    expect(
      screen.getByText("What is the weather today?", { selector: "pre" }),
    ).toBeInTheDocument()
  })

  it("shows an error when the apply guardrail call fails", async () => {
    mockSend.mockRejectedValue(new Error("guardrail unavailable"))
    renderTester()
    submitContent("hello")
    await expect(
      screen.findByText("guardrail unavailable"),
    ).resolves.toBeInTheDocument()
  })
})
