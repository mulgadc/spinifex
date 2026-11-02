import type { ResourceRecordSet } from "@aws-sdk/client-route-53"
import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import type { ActivationState } from "@/lib/route53-activation"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

import { RecordsTab } from "./records-tab"

const ZONE = "example.com."

const APEX_NS: ResourceRecordSet = {
  Name: ZONE,
  Type: "NS",
  TTL: 172_800,
  ResourceRecords: [{ Value: "ns1.spx3.net." }, { Value: "ns2.spx3.net." }],
}

const APEX_SOA: ResourceRecordSet = {
  Name: ZONE,
  Type: "SOA",
  TTL: 900,
  ResourceRecords: [
    { Value: "ns1.spx3.net. hostmaster.spx3.net. 1 7200 900 1209600 86400" },
  ],
}

const WWW: ResourceRecordSet = {
  Name: "www.example.com.",
  Type: "A",
  TTL: 300,
  ResourceRecords: [{ Value: "192.0.2.10" }],
}

const SUB_NS: ResourceRecordSet = {
  Name: "sub.example.com.",
  Type: "NS",
  TTL: 300,
  ResourceRecords: [{ Value: "ns.elsewhere.example." }],
}

const TXT: ResourceRecordSet = {
  Name: "example.com.",
  Type: "TXT",
  TTL: 300,
  ResourceRecords: [{ Value: '"hello \\"world\\""' }],
}

function renderTab(
  recordSets: ResourceRecordSet[] = [APEX_NS, APEX_SOA, WWW],
  activationState: ActivationState | undefined = "ACTIVE",
) {
  const qc = createTestQueryClient()
  qc.setQueryData(["route53", "hostedZones", "Z1", "recordSets"], recordSets)

  renderWithClient(
    <RecordsTab
      activationState={activationState}
      zoneId="Z1"
      zoneName={ZONE}
    />,
    qc,
  )
}

function changeCall() {
  return sdk.send.mock.calls.find(
    ([command]) =>
      command.constructor.name === "ChangeResourceRecordSetsCommand",
  )?.[0]
}

// The success refetches the record list, so the batch is found by command
// rather than taken as the last call.
async function submittedChanges(): Promise<unknown> {
  await waitFor(() => {
    expect(changeCall()).toBeDefined()
  })

  const input = changeCall()?.input as
    | { ChangeBatch: { Changes: unknown } }
    | undefined

  return input?.ChangeBatch.Changes
}

describe("RecordsTab", () => {
  beforeEach(() => {
    sdk.reset()
    sdk.setHandler("ChangeResourceRecordSetsCommand", () => ({
      ChangeInfo: { Id: "/change/C1", Status: "PENDING" },
    }))
    sdk.setHandler("ListResourceRecordSetsCommand", () => ({
      ResourceRecordSets: [],
    }))
    sdk.setHandler("ListHostedZonesCommand", () => ({ HostedZones: [] }))
  })

  it("keeps the apex NS and SOA out of selection and editing", async () => {
    renderTab([APEX_NS, APEX_SOA, WWW, SUB_NS])
    const user = userEvent.setup()

    expect(screen.getAllByText("Managed by the zone")).toHaveLength(2)
    expect(screen.queryByLabelText(`Select ${ZONE}|NS`)).toBeNull()
    expect(screen.queryByLabelText(`Select ${ZONE}|SOA`)).toBeNull()
    expect(
      screen.getByLabelText("Select www.example.com.|A"),
    ).toBeInTheDocument()
    // A delegation below the apex is the customer's.
    expect(
      screen.getByLabelText("Select sub.example.com.|NS"),
    ).toBeInTheDocument()

    await user.click(screen.getByText("ns1.spx3.net., ns2.spx3.net."))
    expect(screen.queryByRole("region", { name: "Edit record" })).toBeNull()
  })

  it("opens a delegation's NS record for editing on a row click", async () => {
    renderTab([APEX_NS, SUB_NS])
    const user = userEvent.setup()

    await user.click(screen.getByText("sub.example.com."))

    const panel = await screen.findByRole("region", { name: "Edit record" })
    expect(within(panel).getByLabelText("Name server")).toHaveValue(
      "ns.elsewhere.example.",
    )
  })

  it("warns that records are not served while the zone is pending", () => {
    renderTab(undefined, "PENDING_NAMESERVERS")

    expect(
      screen.getByText(/Records are not served until the domain is verified/),
    ).toBeInTheDocument()
  })

  it("does not warn once the zone is active", () => {
    renderTab(undefined, "ACTIVE")

    expect(screen.queryByText(/Records are not served/)).toBeNull()
  })

  it("creates a TXT record, quoting the plain-text value", async () => {
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add record" }))

    const panel = await screen.findByRole("region", {
      name: "Add record",
    })

    await user.type(within(panel).getByLabelText("Record name"), "_dmarc")
    await user.click(
      within(panel).getByRole("combobox", { name: "Record type" }),
    )
    await user.click(await screen.findByRole("option", { name: "TXT" }))
    await user.type(
      within(panel).getByLabelText("Content"),
      'v=DMARC1; p=none; say "hi"',
    )
    await user.click(within(panel).getByRole("button", { name: "Add record" }))

    await expect(submittedChanges()).resolves.toStrictEqual([
      {
        Action: "CREATE",
        ResourceRecordSet: {
          Name: "_dmarc.example.com.",
          Type: "TXT",
          TTL: 300,
          ResourceRecords: [
            { Value: String.raw`"v=DMARC1; p=none; say \"hi\""` },
          ],
        },
      },
    ])
  })

  it("refuses an invalid value without calling the API", async () => {
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add record" }))

    const panel = await screen.findByRole("region", {
      name: "Add record",
    })

    await user.type(within(panel).getByLabelText("IPv4 address"), "not-an-ip")
    await user.click(within(panel).getByRole("button", { name: "Add record" }))

    await expect(
      within(panel).findByText('"not-an-ip" is not an IPv4 address'),
    ).resolves.toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("edits values in place with an UPSERT", async () => {
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByText("www.example.com."))

    const panel = await screen.findByRole("region", {
      name: "Edit record",
    })

    const values = within(panel).getByLabelText("IPv4 address")
    expect(values).toHaveValue("192.0.2.10")
    await user.clear(values)
    await user.type(values, "192.0.2.20{enter}192.0.2.21")
    await user.click(
      within(panel).getByRole("button", { name: "Save changes" }),
    )

    await expect(submittedChanges()).resolves.toStrictEqual([
      {
        Action: "UPSERT",
        ResourceRecordSet: {
          Name: "www.example.com.",
          Type: "A",
          TTL: 300,
          ResourceRecords: [{ Value: "192.0.2.20" }, { Value: "192.0.2.21" }],
        },
      },
    ])
  })

  it("renames a record as a delete and a create in one batch", async () => {
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByText("www.example.com."))

    const panel = await screen.findByRole("region", {
      name: "Edit record",
    })

    const name = within(panel).getByLabelText("Record name")
    await user.clear(name)
    await user.type(name, "web")
    await user.click(
      within(panel).getByRole("button", { name: "Save changes" }),
    )

    await expect(submittedChanges()).resolves.toStrictEqual([
      { Action: "DELETE", ResourceRecordSet: WWW },
      {
        Action: "CREATE",
        ResourceRecordSet: {
          Name: "web.example.com.",
          Type: "A",
          TTL: 300,
          ResourceRecords: [{ Value: "192.0.2.10" }],
        },
      },
    ])
  })

  it("shows a TXT record's value unquoted in the editor", async () => {
    renderTab([APEX_NS, TXT])
    const user = userEvent.setup()

    await user.click(screen.getByText('"hello \\"world\\""'))

    const panel = await screen.findByRole("region", {
      name: "Edit record",
    })

    expect(within(panel).getByLabelText("Content")).toHaveValue('hello "world"')
    expect(within(panel).getByLabelText("Record name")).toHaveValue("")
  })

  it("deletes the selected sets in one batch, exactly as the API returned them", async () => {
    renderTab([APEX_NS, APEX_SOA, WWW, SUB_NS])
    const user = userEvent.setup()

    await user.click(screen.getByLabelText("Select www.example.com.|A"))
    await user.click(screen.getByLabelText("Select sub.example.com.|NS"))
    await user.click(screen.getByRole("button", { name: "Delete" }))
    await user.click(await screen.findByRole("button", { name: "Delete 2" }))

    await expect(submittedChanges()).resolves.toStrictEqual([
      { Action: "DELETE", ResourceRecordSet: WWW },
      { Action: "DELETE", ResourceRecordSet: SUB_NS },
    ])
  })

  it("selecting every row leaves the zone's own records out", async () => {
    renderTab([APEX_NS, APEX_SOA, WWW])
    const user = userEvent.setup()

    await user.click(screen.getByLabelText("Select all rows"))

    expect(screen.getByText("1 selected")).toBeInTheDocument()
  })

  it("keeps a TTL that is not a preset when the record is saved", async () => {
    renderTab([APEX_NS, { ...WWW, TTL: 45 }])
    const user = userEvent.setup()

    await user.click(screen.getByText("www.example.com."))

    const panel = await screen.findByRole("region", { name: "Edit record" })
    expect(
      within(panel).getByRole("combobox", { name: "TTL" }),
    ).toHaveTextContent("45 seconds")
    await user.click(
      within(panel).getByRole("button", { name: "Save changes" }),
    )

    await expect(submittedChanges()).resolves.toStrictEqual([
      { Action: "UPSERT", ResourceRecordSet: { ...WWW, TTL: 45 } },
    ])
  })

  it("picks a TTL from the presets, defaulting to Auto", async () => {
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add record" }))

    const panel = await screen.findByRole("region", { name: "Add record" })
    const ttl = within(panel).getByRole("combobox", { name: "TTL" })
    expect(ttl).toHaveTextContent("Auto (5 min)")

    await user.click(ttl)
    await user.click(await screen.findByRole("option", { name: "1 hr" }))
    await user.type(within(panel).getByLabelText("Record name"), "api")
    await user.type(within(panel).getByLabelText("IPv4 address"), "192.0.2.5")
    await user.click(within(panel).getByRole("button", { name: "Add record" }))

    await expect(submittedChanges()).resolves.toMatchObject([
      { ResourceRecordSet: { Name: "api.example.com.", TTL: 3600 } },
    ])
  })

  it("asks for an MX priority and puts it before the mail server", async () => {
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add record" }))

    const panel = await screen.findByRole("region", { name: "Add record" })
    expect(within(panel).queryByLabelText("Priority")).toBeNull()

    await user.click(
      within(panel).getByRole("combobox", { name: "Record type" }),
    )
    await user.click(await screen.findByRole("option", { name: "MX" }))

    const priority = within(panel).getByLabelText("Priority")
    await user.clear(priority)
    await user.type(priority, "5")
    await user.type(
      within(panel).getByLabelText("Mail server"),
      "mx.example.com",
    )
    await user.click(within(panel).getByRole("button", { name: "Add record" }))

    await expect(submittedChanges()).resolves.toMatchObject([
      {
        ResourceRecordSet: {
          Type: "MX",
          ResourceRecords: [{ Value: "5 mx.example.com" }],
        },
      },
    ])
  })

  it("explains each record type beside the picker", async () => {
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add record" }))

    const panel = await screen.findByRole("region", { name: "Add record" })
    await user.click(
      within(panel).getByRole("button", { name: "About Record type" }),
    )

    await expect(
      screen.findByText("Delegates a subdomain to other name servers."),
    ).resolves.toBeInTheDocument()
  })

  it("shows the API's refusal in the form and keeps it open", async () => {
    sdk.setHandler("ChangeResourceRecordSetsCommand", () => {
      throw Object.assign(
        new Error("Tried to create resource record set but it already exists"),
        {
          name: "InvalidChangeBatch",
        },
      )
    })
    renderTab()
    const user = userEvent.setup()

    await user.click(screen.getByRole("button", { name: "Add record" }))

    const panel = await screen.findByRole("region", {
      name: "Add record",
    })

    await user.type(within(panel).getByLabelText("Record name"), "www")
    await user.type(within(panel).getByLabelText("IPv4 address"), "192.0.2.10")
    await user.click(within(panel).getByRole("button", { name: "Add record" }))

    await expect(
      within(panel).findByText(/InvalidChangeBatch: Tried to create/),
    ).resolves.toBeInTheDocument()
    expect(
      screen.getByRole("region", { name: "Add record" }),
    ).toBeInTheDocument()
  })
})
