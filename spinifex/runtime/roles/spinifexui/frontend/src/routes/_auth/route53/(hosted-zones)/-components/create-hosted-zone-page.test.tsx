import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { HostedZoneActivation } from "@/lib/route53-activation"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

const activation = vi.hoisted(() => ({ get: vi.fn(), verify: vi.fn() }))

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

import { CreateHostedZonePage } from "./create-hosted-zone-page"

const onClose = vi.fn()

const NAME_SERVERS = ["ns1.spx3.net", "ns2.spx3.net"]

const PENDING: HostedZoneActivation = {
  hostedZoneId: "Z1",
  state: "PENDING_NAMESERVERS",
  reason: "the parent delegates to other name servers",
  nameServers: NAME_SERVERS,
  checkedAt: "2026-10-27T01:02:03Z",
  delegatedTo: ["ns1.registrar.example"],
  delegationSource: "a.gtld-servers.net",
}

const ACTIVE: HostedZoneActivation = {
  ...PENDING,
  state: "ACTIVE",
  reason: undefined,
  delegatedTo: NAME_SERVERS,
}

function createdZone() {
  return {
    HostedZone: {
      Id: "/hostedzone/Z1",
      Name: "example.com.",
      CallerReference: "ref",
    },
    DelegationSet: { NameServers: NAME_SERVERS },
    ChangeInfo: { Id: "/change/C1", Status: "PENDING" },
  }
}

type User = ReturnType<typeof userEvent.setup>

async function addDomain(user: User) {
  await user.type(screen.getByLabelText("Domain name"), "example.com")
  await user.type(screen.getByLabelText("Comment (optional)"), "prod site")
  await user.click(screen.getByRole("button", { name: "Add domain" }))
}

describe("create-hosted-zone walkthrough", () => {
  beforeEach(() => {
    sdk.reset()
    sdk.setHandler("ListHostedZonesCommand", () => ({ HostedZones: [] }))
    activation.get.mockReset()
    activation.verify.mockReset()
    onClose.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it("shows the API's refusal inline and stays on the domain step", async () => {
    sdk.setHandler("CreateHostedZoneCommand", () => {
      throw Object.assign(new Error("spx3.net is reserved"), {
        name: "InvalidDomainName",
      })
    })
    renderWithClient(
      <CreateHostedZonePage onClose={onClose} />,
      createTestQueryClient(),
    )
    const user = userEvent.setup()

    await addDomain(user)

    await expect(
      screen.findByText("InvalidDomainName: spx3.net is reserved"),
    ).resolves.toBeInTheDocument()
    expect(
      screen.getByText("The domain could not be added"),
    ).toBeInTheDocument()
    expect(screen.getByLabelText("Domain name")).toBeEnabled()
    expect(onClose).not.toHaveBeenCalled()
  })

  it("refuses a malformed domain before calling the API", async () => {
    renderWithClient(
      <CreateHostedZonePage onClose={onClose} />,
      createTestQueryClient(),
    )
    const user = userEvent.setup()

    await user.type(screen.getByLabelText("Domain name"), "localhost")
    await user.click(screen.getByRole("button", { name: "Add domain" }))

    await expect(
      screen.findByText(/Enter a domain name such as example.com/),
    ).resolves.toBeInTheDocument()
    expect(sdk.send).not.toHaveBeenCalled()
  })

  it("creates the zone and lists its name servers with copy buttons", async () => {
    sdk.setHandler("CreateHostedZoneCommand", createdZone)
    renderWithClient(
      <CreateHostedZonePage onClose={onClose} />,
      createTestQueryClient(),
    )
    const user = userEvent.setup()

    await addDomain(user)

    await expect(screen.findByText("ns1.spx3.net")).resolves.toBeInTheDocument()
    expect(sdk.send.mock.calls[0]?.[0].input).toMatchObject({
      Name: "example.com",
      HostedZoneConfig: { Comment: "prod site" },
    })
    expect(screen.getByText("ns2.spx3.net")).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: "Copy ns2.spx3.net" }))
    await expect(navigator.clipboard.readText()).resolves.toBe("ns2.spx3.net")
    expect(activation.get).not.toHaveBeenCalled()

    await user.click(
      screen.getByRole("button", { name: "Skip and add records now" }),
    )
    expect(onClose).toHaveBeenCalledWith("Z1")
  })

  it("shows the parent's answer while pending and Registered after Check now", async () => {
    sdk.setHandler("CreateHostedZoneCommand", createdZone)
    activation.get.mockResolvedValue(PENDING)
    activation.verify.mockResolvedValue(ACTIVE)
    renderWithClient(
      <CreateHostedZonePage onClose={onClose} />,
      createTestQueryClient(),
    )
    const user = userEvent.setup()

    await addDomain(user)
    await user.click(
      await screen.findByRole("button", {
        name: "I have updated my name servers",
      }),
    )

    await expect(
      screen.findByText("the parent delegates to other name servers"),
    ).resolves.toBeInTheDocument()
    expect(screen.getByText("ns1.registrar.example")).toBeInTheDocument()
    expect(
      screen.getByText("Answered by a.gtld-servers.net"),
    ).toBeInTheDocument()
    expect(screen.getByText("Pending nameservers")).toBeInTheDocument()
    expect(screen.queryByText("Registered")).toBeNull()

    await user.click(screen.getByRole("button", { name: "Check now" }))

    await expect(screen.findByText("Registered")).resolves.toBeInTheDocument()
    expect(activation.verify).toHaveBeenCalledWith("Z1")
    await user.click(screen.getByRole("button", { name: "Go to the zone" }))
    expect(onClose).toHaveBeenCalledWith("Z1")
  })

  it("polls every 5 s and turns Registered when the delegation lands", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    sdk.setHandler("CreateHostedZoneCommand", createdZone)
    activation.get.mockResolvedValue(PENDING)
    renderWithClient(
      <CreateHostedZonePage onClose={onClose} />,
      createTestQueryClient(),
    )
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })

    await addDomain(user)
    await user.click(
      await screen.findByRole("button", {
        name: "I have updated my name servers",
      }),
    )
    await expect(
      screen.findByText("Pending nameservers"),
    ).resolves.toBeInTheDocument()
    const callsWhilePending = activation.get.mock.calls.length

    activation.get.mockResolvedValue(ACTIVE)
    await vi.advanceTimersByTimeAsync(5000)

    await expect(screen.findByText("Registered")).resolves.toBeInTheDocument()
    expect(activation.get.mock.calls.length).toBeGreaterThan(callsWhilePending)

    // Active stops the polling.
    const callsWhenActive = activation.get.mock.calls.length
    await vi.advanceTimersByTimeAsync(15_000)
    await waitFor(() => {
      expect(activation.get).toHaveBeenCalledTimes(callsWhenActive)
    })
  })
})
