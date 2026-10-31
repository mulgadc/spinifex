import type { Cluster } from "@aws-sdk/client-eks"
import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import type { PanelTab } from "@/lib/panel-search"
import { createTestQueryClient, renderWithClient } from "@/test/render"

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(vi.fn())
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { ClusterDetails } from "./cluster-details"

const CLUSTER = "demo"

function seed(cluster: Cluster) {
  const qc = createTestQueryClient()
  qc.setQueryData(["eks", "clusters", CLUSTER], { cluster })
  qc.setQueryData(["eks", "clusters", CLUSTER, "nodegroups"], {
    nodegroups: [],
  })
  qc.setQueryData(["eks", "clusters", CLUSTER, "addons"], { addons: [] })
  qc.setQueryData(["eks", "clusters", CLUSTER, "access-entries"], {
    accessEntries: [],
  })

  return qc
}

const ACTIVE: Cluster = {
  arn: "arn:aws:eks:::cluster/demo",
  status: "ACTIVE",
  version: "1.29",
  platformVersion: "eks.1",
  endpoint: "https://api.demo",
  resourcesVpcConfig: { vpcId: "vpc-1" },
  tags: { team: "platform" },
}

function renderDetails(
  cluster: Cluster,
  {
    tab = "overview",
    onClose = vi.fn(),
  }: { tab?: PanelTab; onClose?: () => void } = {},
) {
  renderWithClient(
    <ClusterDetails
      clusterName={CLUSTER}
      layout="page"
      onClose={onClose}
      onTabChange={vi.fn()}
      tab={tab}
    />,
    seed(cluster),
  )
}

describe("ClusterDetails", () => {
  it("shows the cluster, its node groups, networking and kubectl commands in Overview", () => {
    renderDetails(ACTIVE)
    expect(screen.getByText("arn:aws:eks:::cluster/demo")).toBeInTheDocument()
    expect(screen.getByText("https://api.demo")).toBeInTheDocument()
    expect(screen.getByText("No node groups.")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "vpc-1" })).toBeInTheDocument()
    fireEvent.click(screen.getByRole("button", { name: "AWS CLI" }))
    expect(screen.getByText(/update-kubeconfig/u)).toBeInTheDocument()
  })

  it("surfaces cluster health issues beside the status", () => {
    renderDetails({
      status: "ACTIVE",
      health: {
        issues: [
          {
            code: "ClusterUnreachable",
            message: "apiserver healthz=fail: etcd:unreachable",
          },
        ],
      },
    })
    expect(screen.getAllByText("ACTIVE").length).toBeGreaterThan(0)
    expect(screen.getByText("1 health issue")).toBeInTheDocument()
    expect(
      screen.getByText("apiserver healthz=fail: etcd:unreachable"),
    ).toBeInTheDocument()
  })

  it("falls back to UNKNOWN status when the cluster has none", () => {
    renderDetails({})
    expect(screen.getByText("UNKNOWN")).toBeInTheDocument()
  })

  it("keeps add-ons, access entries, tags and delete in Settings", () => {
    renderDetails(ACTIVE, { tab: "settings" })
    expect(screen.getByText("No add-ons installed.")).toBeInTheDocument()
    expect(screen.getByText("No access entries.")).toBeInTheDocument()
    expect(screen.getByText("platform")).toBeInTheDocument()
    fireEvent.click(screen.getByRole("button", { name: /Delete cluster/u }))
    expect(
      screen.getByText(/permanently deletes cluster "demo"/u),
    ).toBeInTheDocument()
  })

  it("goes back to the list from its own page", () => {
    const onClose = vi.fn()
    renderDetails(ACTIVE, { onClose })
    fireEvent.click(screen.getByRole("button", { name: "Back to clusters" }))
    expect(onClose).toHaveBeenCalledOnce()
  })
})
