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

import { RepositoryPolicyEditor } from "./repository-policy-editor"

const REPO = "team/app"
const POLICY = `{"Version":"2012-10-17"}`

function seed(policyText: string | null) {
  const qc = createTestQueryClient()
  qc.setQueryData(["ecr", "repositories", REPO, "policy"], policyText)
  return qc
}

describe("RepositoryPolicyEditor", () => {
  it("seeds the editor from the stored policy and disables delete when empty", () => {
    renderWithClient(
      <RepositoryPolicyEditor repositoryName={REPO} />,
      seed(null),
    )
    expect(screen.getByRole("button", { name: "Delete" })).toBeDisabled()
  })

  it("saves the edited policy document", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <RepositoryPolicyEditor repositoryName={REPO} />,
      seed(POLICY),
    )
    const textarea = screen.getByRole("textbox")
    expect(textarea).toHaveValue(POLICY)

    fireEvent.change(textarea, { target: { value: `${POLICY} ` } })
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    // First send is SetRepositoryPolicy; a second GetRepositoryPolicy fires from
    // the onSuccess cache invalidation, so assert the first call's payload.
    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0][0].input).toStrictEqual({
      repositoryName: REPO,
      policyText: `${POLICY} `,
    })
  })

  it("deletes the policy when one is attached", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <RepositoryPolicyEditor repositoryName={REPO} />,
      seed(POLICY),
    )
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0][0].input).toStrictEqual({
      repositoryName: REPO,
    })
  })
})
