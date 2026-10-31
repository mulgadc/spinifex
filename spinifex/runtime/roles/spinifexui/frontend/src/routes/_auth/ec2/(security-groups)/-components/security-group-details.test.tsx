import type { SecurityGroup } from "@aws-sdk/client-ec2"
import { screen, waitFor, within } from "@testing-library/react"
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

import { CLOSED_PANEL, type PanelTab } from "@/lib/panel-search"

import { SecurityGroupDetails } from "./security-group-details"
import { SecurityGroupsPage } from "./security-groups-page"

const DEFAULT_GROUP: SecurityGroup = {
  GroupId: "sg-default",
  GroupName: "default",
  VpcId: "vpc-1",
  IpPermissions: [
    {
      IpProtocol: "-1",
      UserIdGroupPairs: [{ GroupId: "sg-default" }],
    },
    {
      IpProtocol: "tcp",
      FromPort: 443,
      ToPort: 443,
      Ipv6Ranges: [{ CidrIpv6: "::/0" }],
    },
  ],
  IpPermissionsEgress: [
    { IpProtocol: "-1", IpRanges: [{ CidrIp: "0.0.0.0/0" }] },
  ],
}

function renderDetails(
  sg: SecurityGroup = DEFAULT_GROUP,
  tab: PanelTab = "overview",
) {
  sdk.send.mockClear()
  renderWithClient(
    <SecurityGroupDetails
      onClose={vi.fn()}
      onTabChange={vi.fn()}
      sg={sg}
      tab={tab}
    />,
    createTestQueryClient(),
  )
}

function section(name: RegExp) {
  const trigger = screen.getByRole("button", { name })
  const card = trigger.closest(".rounded-lg")

  if (!(card instanceof HTMLElement)) {
    throw new TypeError("section not found")
  }

  return within(card)
}

describe("SecurityGroupDetails", () => {
  it("reads each rule as ports and its source, naming a self reference", () => {
    renderDetails()

    const inbound = section(/^Inbound rules \(2\)/u)
    expect(inbound.getByText("All traffic")).toBeInTheDocument()
    expect(
      inbound.getByRole("link", { name: "this security group" }),
    ).toHaveAttribute("href", "/ec2/describe-security-groups/sg-default")
    expect(inbound.getByText("TCP 443")).toBeInTheDocument()
    expect(inbound.getByText("::/0")).toBeInTheDocument()

    const outbound = section(/^Outbound rules \(1\)/u)
    expect(outbound.getByText("0.0.0.0/0")).toBeInTheDocument()
  })

  it("keeps an IPv6 rule visible but offers no revoke for it", () => {
    renderDetails()

    const removes = section(/^Inbound rules/u).getAllByRole("button", {
      name: "Remove rule",
    })

    expect(removes[0]).toBeEnabled()
    expect(removes[1]).toBeDisabled()
  })

  it("revokes a group-sourced rule by its group pair", async () => {
    const user = userEvent.setup()
    renderDetails()

    const [selfRule] = section(/^Inbound rules/u).getAllByRole("button", {
      name: "Remove rule",
    })

    if (!selfRule) {
      throw new Error("no rule")
    }

    await user.click(selfRule)
    await user.click(screen.getByRole("button", { name: "Remove" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: {
        GroupId: "sg-default",
        IpPermissions: [
          {
            IpProtocol: "-1",
            UserIdGroupPairs: [{ GroupId: "sg-default" }],
          },
        ],
      },
    })
  })

  it("adds an inbound rule from its form", async () => {
    const user = userEvent.setup()
    renderDetails()

    await user.click(screen.getByRole("button", { name: "Add inbound rule" }))

    const form = within(
      screen.getByRole("region", { name: "Add inbound rule" }),
    )

    const from = form.getByLabelText("From port")
    const to = form.getByLabelText("To port")

    await user.clear(from)
    await user.type(from, "22")
    await user.clear(to)
    await user.type(to, "22")
    await user.click(form.getByRole("button", { name: "Add rule" }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: {
        GroupId: "sg-default",
        IpPermissions: [
          {
            IpProtocol: "tcp",
            FromPort: 22,
            ToPort: 22,
            IpRanges: [{ CidrIp: "0.0.0.0/0" }],
          },
        ],
      },
    })
  })
})

describe("SecurityGroupDetails delete", () => {
  it("refuses to delete the default group and says why", async () => {
    const user = userEvent.setup()
    renderDetails(DEFAULT_GROUP, "settings")

    const remove = screen.getByRole("button", { name: "Delete security group" })
    expect(remove).toBeDisabled()
    expect(
      screen.getByText("The default security group cannot be deleted."),
    ).toBeInTheDocument()

    await user.click(remove)
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
  })

  it("deletes any other group once confirmed", async () => {
    const user = userEvent.setup()
    renderDetails(
      { ...DEFAULT_GROUP, GroupId: "sg-web", GroupName: "web" },
      "settings",
    )

    expect(
      screen.queryByText("The default security group cannot be deleted."),
    ).not.toBeInTheDocument()
    await user.click(
      screen.getByRole("button", { name: "Delete security group" }),
    )
    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: /^Delete/u }))

    await waitFor(() => {
      expect(sdk.send).toHaveBeenCalled()
    })
    expect(sdk.send.mock.calls[0]?.[0]).toMatchObject({
      input: { GroupId: "sg-web" },
    })
  })
})

describe("SecurityGroupsPage", () => {
  it("counts rules per source and opens the panel for a clicked row", async () => {
    const user = userEvent.setup()
    const onSearchChange = vi.fn()
    renderWithClient(
      <SecurityGroupsPage
        onSearchChange={onSearchChange}
        search={CLOSED_PANEL}
        securityGroups={[DEFAULT_GROUP]}
      />,
      createTestQueryClient(),
    )

    const row = screen.getByRole("row", { name: /sg-default/u })
    expect(
      within(row)
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toStrictEqual(expect.arrayContaining(["2", "1"]))

    await user.click(within(row).getByRole("cell", { name: "vpc-1" }))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "sg-default",
    })
  })
})
