import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, within } from "@testing-library/react"
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

  it("offers uninstalled catalog add-ons in the add dialog", () => {
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Add add-on" }))
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

  it("shows a refused add in the dialog and keeps it open", async () => {
    send.mockRejectedValueOnce(
      Object.assign(new Error("The service account role is required."), {
        name: "InvalidParameterException",
      }),
    )
    renderWithClient(<AddonsTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Add add-on" }))
    fireEvent.change(screen.getByLabelText("Add-on"), {
      target: { value: "aws-load-balancer-controller" },
    })
    const dialog = screen.getByRole("alertdialog")
    fireEvent.click(within(dialog).getByRole("button", { name: "Add" }))

    await expect(
      within(dialog).findByText(
        "InvalidParameterException: The service account role is required.",
      ),
    ).resolves.toBeInTheDocument()
    expect(send.mock.calls[0]?.[0].input.addonName).toBe(
      "aws-load-balancer-controller",
    )
    expect(screen.getByLabelText("Add-on")).toHaveValue(
      "aws-load-balancer-controller",
    )
    expect(within(dialog).getByRole("button", { name: "Add" })).toBeEnabled()
  })
})
