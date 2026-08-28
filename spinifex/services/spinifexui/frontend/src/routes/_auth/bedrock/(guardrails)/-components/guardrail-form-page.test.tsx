import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({ guardrailId: "gr-new" })
const mockNavigate = vi.fn()

vi.mock("@/lib/awsClient", () => ({
  getBedrockClient: () => ({ send: mockSend }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    useNavigate: () => mockNavigate,
    Link: ({ children, to }: { children: ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"
import {
  EMPTY_GUARDRAIL_FORM_DEFAULTS,
  type GuardrailFormData,
} from "@/types/bedrock"

import { GuardrailFormPage } from "./guardrail-form-page"

function renderCreate() {
  return renderWithClient(
    <GuardrailFormPage
      defaultValues={EMPTY_GUARDRAIL_FORM_DEFAULTS}
      mode="create"
    />,
    createTestQueryClient(),
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

  it("submits CreateGuardrailCommand and navigates to the new guardrail", async () => {
    const user = userEvent.setup()
    renderCreate()

    await user.type(screen.getByLabelText("Name"), "content-safety")
    await user.type(
      screen.getByLabelText("Blocked input message"),
      "Blocked input.",
    )
    await user.type(
      screen.getByLabelText("Blocked output message"),
      "Blocked output.",
    )
    await user.click(screen.getByRole("button", { name: "Create Guardrail" }))

    await screen.findByRole("button", { name: "Create Guardrail" })
    expect(mockSend).toHaveBeenCalled()
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.name).toBe("content-safety")
    expect(mockNavigate).toHaveBeenCalledWith({
      params: { guardrailId: "gr-new" },
      to: "/bedrock/list-guardrails/$guardrailId",
    })
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
