import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

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

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CreateKeyPairPage } from "./create-key-pair-page"

function renderPage() {
  return renderWithClient(<CreateKeyPairPage />, createTestQueryClient())
}

describe("create-key-pair route", () => {
  // Only RSA can wrap a Windows administrator password, so the untouched form
  // has to submit RSA rather than leaving the user to know that.
  it("submits rsa without the user choosing a key type", async () => {
    renderPage()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText("Key Pair Name"), "my-key")
    await user.click(screen.getByRole("button", { name: "Create Key Pair" }))

    await waitFor(() => {
      expect(send).toHaveBeenCalledOnce()
    })
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

    await waitFor(() => {
      expect(send).toHaveBeenCalledOnce()
    })
    expect(send.mock.calls[0]?.[0].input).toStrictEqual({
      KeyName: "my-key",
      KeyType: "ed25519",
    })
  })
})
