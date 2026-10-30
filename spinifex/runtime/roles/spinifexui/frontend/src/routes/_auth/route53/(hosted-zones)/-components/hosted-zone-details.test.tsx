import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
import type { HostedZoneActivation } from "@/lib/route53-activation"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const activation = vi.hoisted(() => ({ get: vi.fn(), verify: vi.fn() }))

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
  verifyHostedZoneDelegation: activation.verify,
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { HostedZoneDetails } from "./hosted-zone-details"

const ZONE = "example.com."

const PENDING: HostedZoneActivation = {
  hostedZoneId: "Z1",
  state: "PENDING_NAMESERVERS",
  reason: "no delegation found",
  nameServers: ["ns1.spx3.net"],
  delegatedTo: [],
}

const WWW = {
  Name: "www.example.com.",
  Type: "A",
  TTL: 300,
  ResourceRecords: [{ Value: "192.0.2.10" }],
}

const HOSTED_ZONE = {
  Id: "/hostedzone/Z1",
  Name: ZONE,
  CallerReference: "r",
  ResourceRecordSetCount: 3,
  Config: { Comment: "marketing", PrivateZone: false },
}

function renderPanel(
  recordSets: unknown[] = [WWW],
  tab: PanelTab = "overview",
) {
  const qc = createTestQueryClient()
  qc.setQueryData(
    ["route53", "hostedZones", "Z1", "recordSets"],
    [
      {
        Name: ZONE,
        Type: "NS",
        TTL: 172_800,
        ResourceRecords: [{ Value: "ns1.spx3.net." }],
      },
      {
        Name: ZONE,
        Type: "SOA",
        TTL: 900,
        ResourceRecords: [{ Value: "ns1.spx3.net. h. 1 2 3 4 5" }],
      },
      ...recordSets,
    ],
  )
  const onClose = vi.fn()
  renderWithClient(
    <HostedZoneDetails
      onClose={onClose}
      onTabChange={vi.fn()}
      tab={tab}
      zone={HOSTED_ZONE}
    />,
    qc,
  )

  return onClose
}

describe("hosted zone details", () => {
  beforeEach(() => {
    sdk.reset()
    sdk.setHandler("ListHostedZonesCommand", () => ({ HostedZones: [] }))
    activation.get.mockResolvedValue(PENDING)
  })

  it("shows the zone, its records and the pending banner", async () => {
    renderPanel()

    expect(screen.getByRole("heading", { name: ZONE })).toBeInTheDocument()
    expect(screen.getByText("marketing")).toBeInTheDocument()
    expect(screen.getByText("www.example.com.")).toBeInTheDocument()
    await expect(
      screen.findByText(/Records are not served until the domain is verified/),
    ).resolves.toBeInTheDocument()
  })

  it("shows the zone ID and the parent's answer on the overview", async () => {
    renderPanel()

    await expect(
      screen.findByText("no delegation found"),
    ).resolves.toBeInTheDocument()
    expect(screen.getAllByText("Z1").length).toBeGreaterThan(0)
    expect(screen.getByText("Not checked yet")).toBeInTheDocument()
    expect(
      screen.getByText(/does not delegate this domain to any name servers yet/),
    ).toBeInTheDocument()
  })

  it("deletes the customer's record sets, then the zone", async () => {
    sdk.setHandler("ChangeResourceRecordSetsCommand", () => ({}))
    sdk.setHandler("DeleteHostedZoneCommand", () => ({}))
    const onClose = renderPanel([WWW], "settings")
    const user = userEvent.setup()

    await user.click(await screen.findByRole("button", { name: "Delete zone" }))
    await expect(
      screen.findByText(
        /Its 1 record set besides NS and SOA will be deleted first/,
      ),
    ).resolves.toBeInTheDocument()
    await user.type(
      await screen.findByLabelText(/^To confirm, type/u),
      "delete",
    )
    await user.click(screen.getByRole("button", { name: "Delete" }))

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })
    const names = sdk.send.mock.calls.map(([c]) => c.constructor.name)
    expect(names.slice(0, 2)).toStrictEqual([
      "ChangeResourceRecordSetsCommand",
      "DeleteHostedZoneCommand",
    ])
    expect(sdk.send.mock.calls[0]?.[0].input).toStrictEqual({
      HostedZoneId: "Z1",
      ChangeBatch: { Changes: [{ Action: "DELETE", ResourceRecordSet: WWW }] },
    })
  })

  it("explains HostedZoneNotEmpty when the zone still holds records", async () => {
    sdk.setHandler("DeleteHostedZoneCommand", () => {
      throw Object.assign(
        new Error(
          "The specified hosted zone contains non-required resource record sets",
        ),
        {
          name: "HostedZoneNotEmpty",
        },
      )
    })
    const onClose = renderPanel([], "settings")
    const user = userEvent.setup()

    await user.click(await screen.findByRole("button", { name: "Delete zone" }))
    await user.type(
      await screen.findByLabelText(/^To confirm, type/u),
      "delete",
    )
    await user.click(await screen.findByRole("button", { name: "Delete" }))

    await expect(
      screen.findByText(/still holds record sets besides its NS and SOA/),
    ).resolves.toBeInTheDocument()
    expect(
      screen.getByText(/HostedZoneNotEmpty: The specified hosted zone/),
    ).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })
})
