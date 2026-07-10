import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { send } = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock("@/lib/awsClient", () => ({
  getIamClient: () => ({ send }),
}))

import { InlinePoliciesPanel } from "./inline-policies-panel"

const USER = "alice"
const POLICY = "s3-read"
const DOCUMENT = '{"Version":"2012-10-17"}'

function seed(policyNames: string[], document?: string) {
  const qc = createTestQueryClient()
  qc.setQueryData(["iam", "user-inline-policies", USER], {
    PolicyNames: policyNames,
  })
  if (document !== undefined) {
    qc.setQueryData(["iam", "user-inline-policies", USER, POLICY], document)
  }
  return qc
}

// The add form renders a name <input> and a JSON <textarea>, both with the
// textbox role, so the editor is selected by tag name.
function jsonEditor(): HTMLTextAreaElement {
  const editor = screen
    .getAllByRole("textbox")
    .find((node) => node.tagName === "TEXTAREA")
  return editor as HTMLTextAreaElement
}

describe("InlinePoliciesPanel", () => {
  it("lists inline policy names", () => {
    renderWithClient(
      <InlinePoliciesPanel kind="user" name={USER} />,
      seed([POLICY]),
    )
    expect(screen.getByText(POLICY)).toBeInTheDocument()
  })

  it("shows an empty state with no inline policies", () => {
    renderWithClient(<InlinePoliciesPanel kind="user" name={USER} />, seed([]))
    expect(screen.getByText("No inline policies.")).toBeInTheDocument()
  })

  it("opens the add form seeded with the policy template", () => {
    renderWithClient(<InlinePoliciesPanel kind="user" name={USER} />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Add inline policy" }))
    expect(screen.getByLabelText("Policy name")).toBeInTheDocument()
    expect(jsonEditor().value).toContain("2012-10-17")
  })

  it("blocks save when the document is invalid JSON", () => {
    renderWithClient(<InlinePoliciesPanel kind="user" name={USER} />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Add inline policy" }))
    fireEvent.change(screen.getByLabelText("Policy name"), {
      target: { value: POLICY },
    })
    fireEvent.change(jsonEditor(), {
      target: { value: "not json" },
    })
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled()
    expect(screen.getByText("Policy must be valid JSON.")).toBeInTheDocument()
  })

  it("creates an inline policy via PutUserPolicy", async () => {
    send.mockResolvedValue({})
    renderWithClient(<InlinePoliciesPanel kind="user" name={USER} />, seed([]))
    fireEvent.click(screen.getByRole("button", { name: "Add inline policy" }))
    fireEvent.change(screen.getByLabelText("Policy name"), {
      target: { value: POLICY },
    })
    fireEvent.change(jsonEditor(), {
      target: { value: DOCUMENT },
    })
    fireEvent.click(screen.getByRole("button", { name: "Save" }))

    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      UserName: USER,
      PolicyName: POLICY,
      PolicyDocument: DOCUMENT,
    })
  })

  it("deletes an inline policy via DeleteUserPolicy", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <InlinePoliciesPanel kind="user" name={USER} />,
      seed([POLICY]),
    )
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      UserName: USER,
      PolicyName: POLICY,
    })
  })

  it("edits an inline policy and saves via PutUserPolicy", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <InlinePoliciesPanel kind="user" name={USER} />,
      seed([POLICY], DOCUMENT),
    )
    fireEvent.click(screen.getByRole("button", { name: "Edit" }))
    const textarea = await screen.findByRole("textbox")
    expect(textarea).toHaveValue(DOCUMENT)

    fireEvent.change(textarea, {
      target: { value: '{"Version":"2012-10-17","Statement":[]}' },
    })
    fireEvent.click(screen.getByRole("button", { name: "Save" }))

    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      UserName: USER,
      PolicyName: POLICY,
      PolicyDocument: '{"Version":"2012-10-17","Statement":[]}',
    })
  })
})
