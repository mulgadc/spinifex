import type { KeyPairInfo } from "@aws-sdk/client-ec2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly input: object
  }

  return {
    sdk: {
      send: vi.fn<(command: SdkCommand) => Promise<unknown>>(async () => ({})),
    },
  }
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"

import { KeyPairDetails } from "./key-pair-details"
import { KeyPairsPage } from "./key-pairs-page"

const KEYS: KeyPairInfo[] = [
  { KeyPairId: "key-2", KeyName: "zeta", KeyType: "ed25519" },
  { KeyPairId: "key-1", KeyName: "alpha", KeyType: "rsa" },
]

describe("KeyPairDetails", () => {
  it("deletes by key pair ID only after it is confirmed, then closes", async () => {
    const user = userEvent.setup()
    const onClose = vi.fn()
    renderWithClient(
      <KeyPairDetails
        keyPair={{ KeyPairId: "key-1", KeyName: "alpha" }}
        onClose={onClose}
        onTabChange={vi.fn()}
        tab="settings"
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("button", { name: "Delete key pair" }))
    expect(sdk.send).not.toHaveBeenCalled()
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { KeyPairId: "key-1" },
    })
  })
})

describe("KeyPairsPage", () => {
  it("lists key pairs by name", () => {
    renderWithClient(
      <KeyPairsPage
        keyPairs={KEYS}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    const names = screen
      .getAllByRole("row")
      .slice(1)
      .map((row) => row.querySelector("a")?.textContent)

    expect(names).toStrictEqual(["alphakey-1", "zetakey-2"])
  })
})
