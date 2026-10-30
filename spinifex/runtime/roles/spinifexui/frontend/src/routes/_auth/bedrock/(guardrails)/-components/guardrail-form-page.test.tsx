import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() =>
  vi.fn().mockResolvedValue({ guardrailId: "gr-new" }),
)

const mockNavigate = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { navigate: mockNavigate })
})

import { createTestQueryClient, renderWithClient } from "@/test/render"
import {
  EMPTY_GUARDRAIL_FORM_DEFAULTS,
  type GuardrailFormData,
} from "@/types/bedrock"

import { GuardrailFormPage } from "./guardrail-form-page"

function renderCreate(onClose = vi.fn()) {
  renderWithClient(
    <GuardrailFormPage
      defaultValues={EMPTY_GUARDRAIL_FORM_DEFAULTS}
      mode="create"
      onClose={onClose}
    />,
    createTestQueryClient(),
  )

  return onClose
}

async function fillRequired(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText("Name"), "content-safety")
  await user.type(
    screen.getByLabelText("Blocked input message"),
    "Blocked input.",
  )
  await user.type(
    screen.getByLabelText("Blocked output message"),
    "Blocked output.",
  )
}

const EDIT_DEFAULTS: GuardrailFormData = {
  ...EMPTY_GUARDRAIL_FORM_DEFAULTS,
  name: "content-safety",
  blockedInputMessaging: "Blocked input.",
  blockedOutputsMessaging: "Blocked output.",
}

function renderEdit() {
  return renderWithClient(
    <GuardrailFormPage
      defaultValues={EDIT_DEFAULTS}
      guardrailId="gr-1"
      mode="edit"
    />,
    createTestQueryClient(),
  )
}

describe("GuardrailFormPage create mode", () => {
  it("renders the required fields and the create submit label", () => {
    renderCreate()
    expect(screen.getByLabelText("Name")).toBeInTheDocument()
    expect(screen.getByLabelText("Blocked input message")).toBeInTheDocument()
    expect(screen.getByLabelText("Blocked output message")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Create Guardrail" }),
    ).toBeInTheDocument()
  })

  it("shows the tags field array only in create mode", () => {
    renderCreate()
    expect(screen.getByText("Tags")).toBeInTheDocument()
  })

  it("submits CreateGuardrailCommand and opens the new guardrail", async () => {
    const user = userEvent.setup()
    const onClose = renderCreate()

    await fillRequired(user)
    await user.click(screen.getByRole("button", { name: "Create Guardrail" }))

    await screen.findByRole("button", { name: "Create Guardrail" })
    expect(mockSend).toHaveBeenCalled()
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.name).toBe("content-safety")
    expect(onClose).toHaveBeenCalledWith("gr-new")
  })

  it("keeps the form open with the error when the create fails", async () => {
    mockSend.mockRejectedValueOnce(new Error("quota reached"))
    const user = userEvent.setup()
    const onClose = renderCreate()

    await fillRequired(user)
    await user.click(screen.getByRole("button", { name: "Create Guardrail" }))

    await expect(
      screen.findByText("Failed to create the guardrail"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByLabelText("Name")).toBeEnabled()
    expect(onClose).not.toHaveBeenCalled()
  })

  it("closes the panel on cancel", async () => {
    const user = userEvent.setup()
    const onClose = renderCreate()

    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(onClose).toHaveBeenCalledWith()
  })
})

describe("GuardrailFormPage edit mode", () => {
  it("prefills the form from defaultValues and hides tags", () => {
    renderEdit()
    expect(screen.getByLabelText("Name")).toHaveValue("content-safety")
    expect(screen.queryByText("Tags")).toBeNull()
    expect(
      screen.getByRole("button", { name: "Save Changes" }),
    ).toBeInTheDocument()
  })

  it("submits UpdateGuardrailCommand with the guardrail id and navigates to the detail route", async () => {
    const user = userEvent.setup()
    renderEdit()

    await user.click(screen.getByRole("button", { name: "Save Changes" }))

    await screen.findByRole("button", { name: "Save Changes" })
    expect(mockSend).toHaveBeenCalled()
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.guardrailIdentifier).toBe("gr-1")
    expect(mockNavigate).toHaveBeenCalledWith({
      params: { guardrailId: "gr-1" },
      to: "/bedrock/list-guardrails/$guardrailId",
    })
  })

  it("sends the managed profanity list when its switch is turned on", async () => {
    const user = userEvent.setup()
    renderEdit()

    const profanity = screen.getByRole("switch", { name: "Block profanity" })
    expect(profanity).not.toBeChecked()
    await user.click(profanity)
    await user.click(screen.getByRole("button", { name: "Save Changes" }))

    await screen.findByRole("button", { name: "Save Changes" })
    const input = mockSend.mock.calls.at(-1)?.[0].input
    expect(input.wordPolicyConfig.managedWordListsConfig).toStrictEqual([
      { type: "PROFANITY" },
    ])
  })

  it("cancels back to the guardrail detail route", async () => {
    const user = userEvent.setup()
    renderEdit()

    await user.click(screen.getByRole("button", { name: "Cancel" }))

    expect(mockNavigate).toHaveBeenCalledWith({
      params: { guardrailId: "gr-1" },
      to: "/bedrock/list-guardrails/$guardrailId",
    })
  })
})
