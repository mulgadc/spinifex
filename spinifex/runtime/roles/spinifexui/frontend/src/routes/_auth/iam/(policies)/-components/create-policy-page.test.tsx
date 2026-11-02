import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { CreatePolicyPage } from "./create-policy-page"

const POLICY_ARN = "arn:aws:iam::000000000001:policy/s3-read"

const onClose = vi.fn()

function submit(name: string) {
  fireEvent.change(screen.getByLabelText("Policy Name"), {
    target: { value: name },
  })
  fireEvent.click(screen.getByRole("button", { name: "Create Policy" }))
}

describe("CreatePolicyPage", () => {
  it("passes the new policy's ARN to onClose", async () => {
    mockSend.mockResolvedValueOnce({
      Policy: { PolicyName: "s3-read", Arn: POLICY_ARN },
    })
    renderWithClient(
      <CreatePolicyPage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("s3-read")

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith(POLICY_ARN)
    })
    expect(mockSend.mock.calls[0]?.[0].input.PolicyName).toBe("s3-read")
  })

  it("shows a refused create beside the button and stays open", async () => {
    mockSend.mockRejectedValueOnce(
      Object.assign(
        new Error("Policy document should not specify a principal."),
        {
          name: "MalformedPolicyDocument",
        },
      ),
    )
    renderWithClient(
      <CreatePolicyPage onClose={onClose} />,
      createTestQueryClient(),
    )

    submit("s3-read")

    await expect(
      screen.findByText(
        "MalformedPolicyDocument: Policy document should not specify a principal.",
      ),
    ).resolves.toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByRole("button", { name: "Create Policy" })).toBeEnabled()
  })
})
