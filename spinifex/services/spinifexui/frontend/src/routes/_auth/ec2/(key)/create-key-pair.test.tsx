import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { send } = vi.hoisted(() => {
  interface Command {
    readonly input: { KeyName?: string; KeyType?: string }
  }
  return {
    send: vi.fn(async (_command: Command) => ({
      KeyMaterial: "-----BEGIN RSA PRIVATE KEY-----",
    })),
  }
})

vi.mock("@/lib/awsClient", () => ({
  getEc2Client: () => ({ send }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    createFileRoute: () => (options: Record<string, unknown>) => options,
    useNavigate: () => vi.fn(),
    Link: ({ children, to }: { children: ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { CreateKeyPair } from "./create-key-pair"

function renderPage() {
  send.mockClear()
  return renderWithClient(<CreateKeyPair />, createTestQueryClient())
}

describe("create-key-pair route", () => {
  // Only RSA can wrap a Windows administrator password, so the untouched form
  // has to submit RSA rather than leaving the user to know that.
  it("submits rsa without the user choosing a key type", async () => {
    renderPage()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText("Key Pair Name"), "my-key")
    await user.click(screen.getByRole("button", { name: "Create Key Pair" }))

    await waitFor(() => expect(send).toHaveBeenCalledTimes(1))
    expect(send.mock.calls[0]?.[0].input).toStrictEqual({
      KeyName: "my-key",
      KeyType: "rsa",
    })
  })

  it("submits the selected key type when the user picks ED25519", async () => {
    renderPage()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText("Key Pair Name"), "my-key")
    await user.click(screen.getByRole("combobox", { name: "Key Type" }))
    await user.click(screen.getByRole("option", { name: "ED25519" }))
    await user.click(screen.getByRole("button", { name: "Create Key Pair" }))

    await waitFor(() => expect(send).toHaveBeenCalledTimes(1))
    expect(send.mock.calls[0]?.[0].input).toStrictEqual({
      KeyName: "my-key",
      KeyType: "ed25519",
    })
  })
})
