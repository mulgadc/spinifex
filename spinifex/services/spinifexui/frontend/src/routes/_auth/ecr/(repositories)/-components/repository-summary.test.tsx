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

import { RepositorySummary } from "./repository-summary"

const REPO = "team/app"
const URI = "111.dkr.ecr.ap-southeast-2.local/team/app"

function render(imageTagMutability: string | undefined) {
  renderWithClient(
    <RepositorySummary
      imageTagMutability={imageTagMutability}
      repositoryName={REPO}
      repositoryUri={URI}
    />,
    createTestQueryClient(),
  )
}

describe("RepositorySummary", () => {
  it("renders the push commands", () => {
    render("MUTABLE")
    expect(screen.getByText(/docker push/)).toBeInTheDocument()
  })

  it("shows Mutable and offers to make immutable when unset", () => {
    render(undefined)
    expect(screen.getByText("Mutable")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: "Make immutable" }),
    ).toBeInTheDocument()
  })

  it("flips a mutable repo to IMMUTABLE", async () => {
    send.mockResolvedValue({})
    render("MUTABLE")
    fireEvent.click(screen.getByRole("button", { name: "Make immutable" }))
    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      repositoryName: REPO,
      imageTagMutability: "IMMUTABLE",
    })
  })

  it("flips an immutable repo back to MUTABLE", async () => {
    send.mockResolvedValue({})
    render("IMMUTABLE")
    expect(screen.getByText("Immutable")).toBeInTheDocument()
    fireEvent.click(screen.getByRole("button", { name: "Make mutable" }))
    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      repositoryName: REPO,
      imageTagMutability: "MUTABLE",
    })
  })
})
