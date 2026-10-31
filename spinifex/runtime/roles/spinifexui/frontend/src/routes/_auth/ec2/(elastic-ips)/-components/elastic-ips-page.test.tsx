import type { Address } from "@aws-sdk/client-ec2"
import { screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const send = vi.hoisted(() =>
  vi.fn(
    async (_command: { constructor: { name: string }; input: object }) => ({}),
  ),
)

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CLOSED_PANEL } from "@/lib/panel-search"

import { ElasticIpsPage } from "./elastic-ips-page"

const ROWS: Address[] = [
  { AllocationId: "eipalloc-1", PublicIp: "198.51.100.1" },
  { AllocationId: "eipalloc-2", PublicIp: "198.51.100.2" },
]

describe("ElasticIpsPage", () => {
  it("releases each selected row with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <ElasticIpsPage
        addresses={ROWS}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(
      screen.getByRole("checkbox", { name: "Select eipalloc-1" }),
    )
    await user.click(
      screen.getByRole("checkbox", { name: "Select eipalloc-2" }),
    )

    const bar = screen.getByRole("toolbar", { name: "Selection actions" })

    expect(bar).toHaveTextContent("2 selected")
    await user.click(within(bar).getByRole("button", { name: "Release" }))

    const dialog = screen.getByRole("alertdialog")

    expect(
      within(dialog).getByRole("list", { name: "Selected Elastic IPs" }),
    ).toHaveTextContent("eipalloc-2")
    await user.click(within(dialog).getByRole("button", { name: "Release 2" }))

    expect(
      send.mock.calls.map(([command]) => [
        command.constructor.name,
        command.input,
      ]),
    ).toStrictEqual([
      ["ReleaseAddressCommand", { AllocationId: "eipalloc-1" }],
      ["ReleaseAddressCommand", { AllocationId: "eipalloc-2" }],
    ])
  })
})
