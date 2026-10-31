import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const send = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

import { AddonsTab } from "./addons-tab"

const CLUSTER = "demo"

function seedCatalog(qc: QueryClient) {
  qc.setQueryData(["eks", "addon-versions"], {
    addons: [
      {
        addonName: "coredns",
        addonVersions: [
          { addonVersion: "v1.0.0", requiresIamPermissions: false },
        ],
      },
      {
        addonName: "aws-load-balancer-controller",
        addonVersions: [
          { addonVersion: "v2.0.0", requiresIamPermissions: true },
        ],
      },
    ],
  })
}

function seed(opts?: { status?: string; issue?: string }): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["eks", "clusters", CLUSTER, "addons"], {
    addons: ["coredns"],
  })
  qc.setQueryData(["eks", "clusters", CLUSTER, "addons", "coredns"], {
    addon: {
      addonName: "coredns",
      status: opts?.status ?? "ACTIVE",
      addonVersion: "v1.0.0",
      serviceAccountRoleArn: "arn:aws:iam::0:role/coredns",
      health: opts?.issue ? { issues: [{ message: opts.issue }] } : undefined,
    },
  })
  seedCatalog(qc)

  return qc
}

describe("AddonsTab", () => {
  it("renders installed add-ons with version and status", () => {
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, seed())
    expect(screen.getByText("coredns")).toBeInTheDocument()
    expect(screen.getByText("v1.0.0")).toBeInTheDocument()
    expect(screen.getByText("ACTIVE")).toBeInTheDocument()
  })

  it("shows empty state when there are no add-ons", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["eks", "clusters", CLUSTER, "addons"], { addons: [] })
    seedCatalog(qc)
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, qc)
    expect(screen.getByText("No add-ons installed.")).toBeInTheDocument()
  })

  it("renders CREATING calmly, not as an error", () => {
    renderWithClient(
      <AddonsTab clusterName={CLUSTER} />,
      seed({ status: "CREATING" }),
    )
    const badge = screen.getByText("CREATING")
    expect(badge).toBeInTheDocument()
    // In-progress renders as the calm secondary variant, not the failure one.
    expect(badge.className).toContain("bg-secondary")
    expect(badge.className).not.toContain("bg-destructive")
  })

  it("surfaces a health issue on a failed add-on", () => {
    renderWithClient(
      <AddonsTab clusterName={CLUSTER} />,
      seed({ status: "DEGRADED", issue: "missing IAM role" }),
    )
    expect(screen.getByText("missing IAM role")).toBeInTheDocument()
  })

  it("offers uninstalled catalog add-ons in the add form", async () => {
    const user = userEvent.setup()
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Add add-on" }))
    const panel = screen.getByRole("region", { name: "Add add-on" })
    await user.click(within(panel).getByLabelText("Add-on"))
    expect(
      screen.getByRole("option", { name: "aws-load-balancer-controller" }),
    ).toBeInTheDocument()
    // coredns is already installed, so it must not be offered again.
    expect(
      screen.queryByRole("option", { name: "coredns" }),
    ).not.toBeInTheDocument()
  })

  it("opens the remove confirmation for an add-on", () => {
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Remove add-on" }))
    expect(
      screen.getByText(/Remove add-on "coredns" from the cluster\?/),
    ).toBeInTheDocument()
  })

  it("updates an add-on with the chosen version and closes the panel", async () => {
    send.mockResolvedValue({})
    const qc = seed()
    qc.setQueryData(["eks", "addon-versions"], {
      addons: [
        {
          addonName: "coredns",
          addonVersions: [
            { addonVersion: "v1.1.0" },
            { addonVersion: "v1.0.0" },
          ],
        },
      ],
    })
    const user = userEvent.setup()
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, qc)
    await user.click(screen.getByRole("button", { name: "Update add-on" }))
    const panel = screen.getByRole("region", { name: "Update coredns" })
    await user.click(within(panel).getByLabelText("Version"))
    await user.click(screen.getByRole("option", { name: "v1.1.0" }))
    await user.click(within(panel).getByRole("button", { name: "Update" }))

    await waitFor(() => {
      expect(
        screen.queryByRole("region", { name: "Update coredns" }),
      ).not.toBeInTheDocument()
    })
    expect(send).toHaveBeenCalledWith(
      expect.objectContaining({
        input: expect.objectContaining({
          clusterName: CLUSTER,
          addonName: "coredns",
          addonVersion: "v1.1.0",
          serviceAccountRoleArn: "arn:aws:iam::0:role/coredns",
        }),
      }),
    )
  })

  it("refuses an update whose configuration is not JSON", async () => {
    const user = userEvent.setup()
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, seed())
    await user.click(screen.getByRole("button", { name: "Update add-on" }))
    const panel = screen.getByRole("region", { name: "Update coredns" })
    fireEvent.change(
      within(panel).getByLabelText("Configuration values (JSON)"),
      { target: { value: "{not json" } },
    )
    await user.click(within(panel).getByRole("button", { name: "Update" }))

    expect(send).not.toHaveBeenCalled()
    expect(
      screen.getByRole("region", { name: "Update coredns" }),
    ).toBeInTheDocument()
  })

  it("shows a refused add in the panel and keeps it open", async () => {
    send.mockRejectedValueOnce(
      Object.assign(new Error("The service account role is required."), {
        name: "InvalidParameterException",
      }),
    )
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, seed())
    const user = userEvent.setup()
    fireEvent.click(screen.getByRole("button", { name: "Add add-on" }))
    const dialog = screen.getByRole("region", { name: "Add add-on" })
    await user.click(within(dialog).getByLabelText("Add-on"))
    await user.click(
      screen.getByRole("option", { name: "aws-load-balancer-controller" }),
    )
    await user.click(within(dialog).getByRole("button", { name: "Add" }))

    await expect(
      within(dialog).findByText(
        "InvalidParameterException: The service account role is required.",
      ),
    ).resolves.toBeInTheDocument()
    expect(send.mock.calls[0]?.[0].input).toMatchObject({
      addonName: "aws-load-balancer-controller",
      addonVersion: "v2.0.0",
    })
    expect(within(dialog).getByLabelText("Add-on")).toHaveTextContent(
      "aws-load-balancer-controller",
    )
    expect(within(dialog).getByRole("button", { name: "Add" })).toBeEnabled()
  })
})
