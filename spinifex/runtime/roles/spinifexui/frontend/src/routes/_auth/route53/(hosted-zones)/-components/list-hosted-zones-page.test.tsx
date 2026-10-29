import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
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

function renderList(
  zones: typeof ZONES = ZONES,
  search: PanelSearch = CLOSED_PANEL,
) {
  const onSearchChange = vi.fn()
  renderWithClient(
    <ListHostedZonesPage
      onSearchChange={onSearchChange}
      search={search}
      zones={zones}
    />,
    createTestQueryClient(),
  )

  return onSearchChange
}

describe("list-hosted-zones page", () => {
  beforeEach(() => {
    sdk.reset()
    sdk.setHandler("ListResourceRecordSetsCommand", () => ({
      ResourceRecordSets: [],
      IsTruncated: false,
    }))
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
    ).toBeInTheDocument()
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

  it("links Add domain to the walkthrough", () => {
    renderList()

    expect(screen.getByRole("link", { name: "Add domain" })).toHaveAttribute(
      "href",
      "/route53/create-hosted-zone",
    )
  })

  it("opens a zone's panel from its row, keyed by the bare zone ID", async () => {
    const onSearchChange = renderList()
    const user = userEvent.setup()

    await user.click(screen.getByText("ZPENDING"))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "ZPENDING",
    })
  })

  it("shows the selected zone in the panel", () => {
    renderList(ZONES, { ...CLOSED_PANEL, selected: "ZACTIVE" })

    expect(
      screen.getByRole("heading", { name: "active.example." }),
    ).toBeInTheDocument()
  })

  it("deletes the selected zones one at a time, each with its own records", async () => {
    sdk.setHandler(
      "ListResourceRecordSetsCommand",
      (input: { HostedZoneId: string }) => ({
        ResourceRecordSets:
          input.HostedZoneId === "ZACTIVE"
            ? [
                { Name: "active.example.", Type: "NS", ResourceRecords: [] },
                { Name: "www.active.example.", Type: "A", ResourceRecords: [] },
              ]
            : [{ Name: "lost.example.", Type: "SOA", ResourceRecords: [] }],
        IsTruncated: false,
      }),
    )
    sdk.setHandler("ChangeResourceRecordSetsCommand", () => ({}))
    sdk.setHandler("DeleteHostedZoneCommand", () => ({}))
    renderList()
    const user = userEvent.setup()

    await user.click(screen.getByRole("checkbox", { name: "Select ZACTIVE" }))
    await user.click(screen.getByRole("checkbox", { name: "Select ZLOST" }))
    await user.click(screen.getByRole("button", { name: "Delete" }))
    expect(screen.getByText("active.example. (ZACTIVE)")).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })

    const writes = sdk.send.mock.calls
      .map(([command]) => command)
      .filter(
        ({ constructor }) =>
          constructor.name !== "ListResourceRecordSetsCommand",
      )
      .map(({ constructor, input }) => [constructor.name, input])

    expect(writes).toStrictEqual([
      [
        "ChangeResourceRecordSetsCommand",
        {
          HostedZoneId: "ZACTIVE",
          ChangeBatch: {
            Changes: [
              {
                Action: "DELETE",
                ResourceRecordSet: {
                  Name: "www.active.example.",
                  Type: "A",
                  ResourceRecords: [],
                },
              },
            ],
          },
        },
      ],
      ["DeleteHostedZoneCommand", { Id: "ZACTIVE" }],
      ["DeleteHostedZoneCommand", { Id: "ZLOST" }],
    ])
  })

  it("explains what to do when there are no zones", () => {
    renderList([])

    expect(screen.getByText("No hosted zones yet.")).toBeInTheDocument()
  })
})
