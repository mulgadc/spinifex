import { screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const { routerState } = vi.hoisted(() => ({
  routerState: { navigate: vi.fn() },
}))

const activation = vi.hoisted(() => ({ get: vi.fn() }))

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@/lib/route53-activation", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/route53-activation")>()),
  getHostedZoneActivation: activation.get,
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal, { navigate: routerState.navigate })
})

import { ListHostedZonesPage } from "./list-hosted-zones-page"

const ZONES = [
  {
    Id: "/hostedzone/ZACTIVE",
    Name: "active.example.",
    CallerReference: "a",
    ResourceRecordSetCount: 7,
    Config: { Comment: "production" },
  },
  {
    Id: "/hostedzone/ZPENDING",
    Name: "pending.example.",
    CallerReference: "b",
    ResourceRecordSetCount: 2,
  },
  {
    Id: "/hostedzone/ZLOST",
    Name: "lost.example.",
    CallerReference: "c",
    ResourceRecordSetCount: 3,
  },
  {
    Id: "/hostedzone/ZSUSP",
    Name: "suspended.example.",
    CallerReference: "d",
    ResourceRecordSetCount: 2,
  },
  {
    Id: "/hostedzone/ZERR",
    Name: "broken.example.",
    CallerReference: "e",
    ResourceRecordSetCount: 2,
  },
]

const STATES = new Map([
  ["ZACTIVE", "ACTIVE"],
  ["ZPENDING", "PENDING_NAMESERVERS"],
  ["ZLOST", "NAMESERVERS_LOST"],
  ["ZSUSP", "SUSPENDED"],
])

function renderList(zones: unknown[] = ZONES) {
  const qc = createTestQueryClient()
  qc.setQueryData(["route53", "hostedZones"], zones)
  renderWithClient(<ListHostedZonesPage />, qc)
}

describe("list-hosted-zones page", () => {
  beforeEach(() => {
    sdk.reset()
    activation.get.mockImplementation(async (id: string) => {
      const state = STATES.get(id)

      if (!state) {
        throw new Error("activation unavailable")
      }

      return { hostedZoneId: id, state, nameServers: [], delegatedTo: [] }
    })
  })

  it("lists each zone with its record count, comment and activation badge", async () => {
    renderList()

    expect(
      screen.getByRole("link", { name: "active.example." }),
    ).toHaveAttribute("href", "/route53/list-hosted-zones/ZACTIVE")
    expect(screen.getByText("7")).toBeInTheDocument()
    expect(screen.getByText("production")).toBeInTheDocument()
    await expect(screen.findByText("Active")).resolves.toBeInTheDocument()
    await expect(
      screen.findByText("Pending nameservers"),
    ).resolves.toBeInTheDocument()
    await expect(
      screen.findByText("Nameservers lost"),
    ).resolves.toBeInTheDocument()
    await expect(screen.findByText("Suspended")).resolves.toBeInTheDocument()
    // A zone whose activation cannot be read still lists, marked Unknown.
    await expect(screen.findByText("Unknown")).resolves.toBeInTheDocument()
    expect(activation.get).toHaveBeenCalledWith("ZPENDING")
  })

  it("opens the walkthrough from Add domain", async () => {
    renderList()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add domain" }))

    expect(routerState.navigate).toHaveBeenCalledWith({
      to: "/route53/create-hosted-zone",
    })
  })

  it("opens a zone from its row", async () => {
    renderList()
    const user = userEvent.setup()

    await user.click(screen.getByText("ZPENDING"))

    expect(routerState.navigate).toHaveBeenCalledWith({
      to: "/route53/list-hosted-zones/$id",
      params: { id: "ZPENDING" },
    })
  })

  it("explains what to do when there are no zones", () => {
    renderList([])

    expect(screen.getByText("No hosted zones yet.")).toBeInTheDocument()
  })
})
