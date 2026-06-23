import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { send } = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock("@/lib/awsClient", () => ({
  getEcrClient: () => ({ send }),
}))

import { LifecyclePolicyEditor } from "./lifecycle-policy-editor"

const REPO = "team/app"
const POLICY = `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":14},"action":{"type":"expire"}}]}`

function seed(policyText: string | null) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecr", "repositories", REPO, "lifecycle"], policyText)
  return qc
}

describe("LifecyclePolicyEditor", () => {
  it("disables delete and preview when no policy is set", () => {
    renderWithClient(
      <LifecyclePolicyEditor repositoryName={REPO} />,
      seed(null),
    )
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled()
    expect(screen.getByRole("button", { name: "Preview" })).toBeDisabled()
  })

  it("saves the edited policy document", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <LifecyclePolicyEditor repositoryName={REPO} />,
      seed(POLICY),
    )
    const textarea = screen.getByRole("textbox")
    expect(textarea).toHaveValue(POLICY)

    fireEvent.change(textarea, { target: { value: `${POLICY} ` } })
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]?.[0].input).toStrictEqual({
      repositoryName: REPO,
      lifecyclePolicyText: `${POLICY} `,
    })
  })

  it("blocks save and flags invalid JSON", () => {
    renderWithClient(
      <LifecyclePolicyEditor repositoryName={REPO} />,
      seed(POLICY),
    )
    fireEvent.change(screen.getByRole("textbox"), {
      target: { value: "{not json" },
    })
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled()
    expect(
      screen.getByText("Lifecycle policy must be valid JSON."),
    ).toBeInTheDocument()
  })

  it("deletes the policy when one is attached", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <LifecyclePolicyEditor repositoryName={REPO} />,
      seed(POLICY),
    )
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]?.[0].input).toStrictEqual({
      repositoryName: REPO,
    })
  })

  it("previews the saved policy and renders the expiry table", async () => {
    send.mockResolvedValue({
      previewResults: [
        {
          imageDigest: "sha256:deadbeefdeadbeef",
          imageTags: ["old"],
          appliedRulePriority: 1,
          action: { type: "EXPIRE" },
        },
      ],
    })
    renderWithClient(
      <LifecyclePolicyEditor repositoryName={REPO} />,
      seed(POLICY),
    )
    fireEvent.click(screen.getByRole("button", { name: "Preview" }))
    await waitFor(() =>
      expect(screen.getByText(/1 image would expire/)).toBeInTheDocument(),
    )
    expect(send.mock.calls[0]?.[0].input).toStrictEqual({
      repositoryName: REPO,
    })
    expect(screen.getByText("old")).toBeInTheDocument()
  })
})
