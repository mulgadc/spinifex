import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const { sdk } = vi.hoisted(() => {
  interface SdkCommand {
    readonly constructor: { name: string }
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

import { DescribeLoadBalancersPage } from "../-components/describe-load-balancers-page"

const LBS: LoadBalancer[] = [
  {
    LoadBalancerArn: "arn:lb:1",
    LoadBalancerName: "lb-one",
    DNSName: "lb-one.example",
    Type: "application",
    Scheme: "internet-facing",
    State: { Code: "active" },
    VpcId: "vpc-aaa",
  },
]

function renderPage(
  loadBalancers: LoadBalancer[],
  onSearchChange = vi.fn(),
  search: PanelSearch = CLOSED_PANEL,
) {
  renderWithClient(
    <DescribeLoadBalancersPage
      loadBalancers={loadBalancers}
      onSearchChange={onSearchChange}
      search={search}
    />,
    createTestQueryClient(),
  )

  return onSearchChange
}

describe("describe-load-balancers list route", () => {
  it("renders load-balancer rows with resolved fields", () => {
    renderPage(LBS)

    expect(screen.getByText("lb-one")).toBeInTheDocument()
    expect(screen.getByText("lb-one.example")).toBeInTheDocument()
    expect(screen.getByText("application")).toBeInTheDocument()
    expect(screen.getByText("internet-facing")).toBeInTheDocument()
    expect(
      screen.getByRole("link", { name: "Create load balancer" }),
    ).toBeInTheDocument()
  })

  it("opens the create form in the side panel", async () => {
    renderPage(LBS, vi.fn(), { ...CLOSED_PANEL, create: true })

    await expect(
      screen.findByRole("button", { name: "Create Load Balancer" }),
    ).resolves.toBeInTheDocument()
    expect(
      screen.getByRole("region", { name: "Create load balancer" }),
    ).toBeInTheDocument()
  })

  it("selects a load balancer by its ARN when its row is clicked", async () => {
    const user = userEvent.setup()
    const onSearchChange = renderPage(LBS)

    await user.click(screen.getByText("internet-facing"))

    expect(onSearchChange).toHaveBeenCalledWith({
      ...CLOSED_PANEL,
      selected: "arn:lb:1",
    })
  })

  it("shows empty state when no load balancers", () => {
    renderPage([])

    expect(screen.getByText("No load balancers yet.")).toBeInTheDocument()
  })

  it("deletes each selected load balancer with its own call", async () => {
    const user = userEvent.setup()
    renderWithClient(
      <DescribeLoadBalancersPage
        loadBalancers={[
          ...LBS,
          {
            ...LBS[0],
            LoadBalancerArn: "arn:lb:2",
            LoadBalancerName: "lb-two",
          },
        ]}
        onSearchChange={vi.fn()}
        search={CLOSED_PANEL}
      />,
      createTestQueryClient(),
    )

    await user.click(screen.getByRole("checkbox", { name: "Select arn:lb:1" }))
    await user.click(screen.getByRole("checkbox", { name: "Select arn:lb:2" }))
    await user.click(
      within(
        screen.getByRole("toolbar", { name: "Selection actions" }),
      ).getByRole("button", { name: "Delete" }),
    )

    const dialog = screen.getByRole("alertdialog")

    expect(within(dialog).getByText("lb-one")).toBeInTheDocument()
    expect(within(dialog).getByText("lb-two")).toBeInTheDocument()

    await user.click(within(dialog).getByRole("button", { name: "Delete 2" }))

    await waitFor(() => {
      expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument()
    })
    expect(
      sdk.send.mock.calls
        .filter(
          ([command]) =>
            command.constructor.name === "DeleteLoadBalancerCommand",
        )
        .map(([command]) => command.input),
    ).toStrictEqual([
      { LoadBalancerArn: "arn:lb:1" },
      { LoadBalancerArn: "arn:lb:2" },
    ])
  })
})
