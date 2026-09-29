import { fireEvent, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import {
  foundationModelsQueryOptions,
  guardrailsQueryOptions,
} from "@/queries/bedrock"
import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const mockSend = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockRuntimeClient: () => ({ send: mockSend }),
}))

import { PlaygroundPage } from "./playground-page"

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(foundationModelsQueryOptions.queryKey, {
    modelSummaries: [
      {
        modelArn: "arn:aws:bedrock:::foundation-model/m1",
        modelId: "m1",
        modelName: "Test Model",
        providerName: "vllm",
      },
    ],
    $metadata: {},
  })
  qc.setQueryData(guardrailsQueryOptions.queryKey, {
    guardrails: [
      {
        id: "gr-1",
        arn: "arn:aws:bedrock:::guardrail/gr-1",
        name: "Test Guardrail",
        status: "READY",
        version: "DRAFT",
        createdAt: new Date("2026-01-01T00:00:00Z"),
        updatedAt: new Date("2026-01-01T00:00:00Z"),
      },
    ],
    $metadata: {},
  })
  return qc
}

async function sendMessage(user: ReturnType<typeof userEvent.setup>) {
  const input = screen.getByPlaceholderText(/^Send a message/)
  await user.type(input, "hello there")
  fireEvent.click(screen.getByRole("button", { name: "Send" }))
}

describe("PlaygroundPage", () => {
  it("renders a completed turn with token counts and an Included badge", async () => {
    mockSend.mockResolvedValue({
      output: { message: { role: "assistant", content: [{ text: "hi" }] } },
      usage: { inputTokens: 12, outputTokens: 34, totalTokens: 46 },
    })
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    await sendMessage(user)

    await expect(screen.findByText("hi")).resolves.toBeInTheDocument()
    expect(screen.getByText("12 in / 34 out")).toBeInTheDocument()
    expect(screen.getByText("Included")).toBeInTheDocument()
  })

  it("appends and mutates only the last turn, leaving earlier turns intact", async () => {
    mockSend
      .mockResolvedValueOnce({
        output: {
          message: { role: "assistant", content: [{ text: "first reply" }] },
        },
        usage: { inputTokens: 1, outputTokens: 2, totalTokens: 3 },
      })
      .mockResolvedValueOnce({
        output: {
          message: { role: "assistant", content: [{ text: "second reply" }] },
        },
        usage: { inputTokens: 4, outputTokens: 5, totalTokens: 9 },
      })
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    await sendMessage(user)
    await expect(screen.findByText("first reply")).resolves.toBeInTheDocument()

    const input = screen.getByPlaceholderText(/^Send a message/)
    await user.type(input, "second message")
    fireEvent.click(screen.getByRole("button", { name: "Send" }))

    await expect(screen.findByText("second reply")).resolves.toBeInTheDocument()
    // The first turn's text is untouched by the second round-trip.
    expect(screen.getByText("first reply")).toBeInTheDocument()
    expect(screen.getByText("hello there")).toBeInTheDocument()
    expect(screen.getByText("second message")).toBeInTheDocument()
  })

  it("renders the warming-up state without throwing on ModelNotReadyException", async () => {
    const error = new Error("model not ready")
    error.name = "ModelNotReadyException"
    mockSend.mockRejectedValue(error)
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    await sendMessage(user)

    await expect(
      screen.findByText("Model is warming up — retry in a moment."),
    ).resolves.toBeInTheDocument()
    // The compose box keeps the text so nothing typed is lost.
    expect(screen.getByPlaceholderText(/^Send a message/)).toHaveValue(
      "hello there",
    )
  })

  it("toggles the raw request/response JSON panel", async () => {
    mockSend.mockResolvedValue({
      output: { message: { role: "assistant", content: [{ text: "hi" }] } },
      usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 },
    })
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    await sendMessage(user)
    await screen.findByText("hi")

    expect(screen.queryByText(/"modelId": "m1"/)).not.toBeInTheDocument()
    await user.click(screen.getByText("Raw request / response"))
    expect(screen.getByText(/"modelId": "m1"/)).toBeInTheDocument()
  })

  it("renders the copy-as-CLI command for bedrock-runtime converse", async () => {
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    await user.click(screen.getByText("AWS CLI"))
    expect(
      screen.getByText("aws bedrock-runtime converse", { exact: false }),
    ).toBeInTheDocument()
  })

  it("sends on Enter and inserts a newline on Shift+Enter", async () => {
    mockSend.mockResolvedValue({
      output: { message: { role: "assistant", content: [{ text: "hi" }] } },
      usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 },
    })
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    const input = screen.getByPlaceholderText(/^Send a message/)
    // Shift+Enter must not send: the text stays in the box.
    await user.type(input, "line one{Shift>}{Enter}{/Shift}line two")
    expect(mockSend).not.toHaveBeenCalled()

    // A bare Enter sends without clicking the button.
    await user.type(input, "{Enter}")
    await expect(screen.findByText("hi")).resolves.toBeInTheDocument()
    expect(mockSend).toHaveBeenCalledOnce()
  })

  it("renders assistant markdown as formatted HTML", async () => {
    mockSend.mockResolvedValue({
      output: {
        message: {
          role: "assistant",
          content: [{ text: "a **bold** word and `inline code`" }],
        },
      },
      usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 },
    })
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    await sendMessage(user)

    const bold = await screen.findByText("bold")
    expect(bold.tagName).toBe("STRONG")
    expect(screen.getByText("inline code").tagName).toBe("CODE")
  })

  it("wires the guardrail selector into the request", async () => {
    mockSend.mockResolvedValue({
      output: { message: { role: "assistant", content: [{ text: "hi" }] } },
      usage: { inputTokens: 1, outputTokens: 1, totalTokens: 2 },
    })
    const user = userEvent.setup()
    renderWithClient(<PlaygroundPage />, seed())

    await user.click(screen.getByLabelText("Guardrail (optional)"))
    await user.click(screen.getByRole("option", { name: "Test Guardrail" }))

    await sendMessage(user)

    await waitFor(() => {
      expect(mockSend.mock.calls[0]?.[0].input.guardrailConfig).toStrictEqual({
        guardrailIdentifier: "gr-1",
        guardrailVersion: "DRAFT",
      })
    })
  })
})
