import { fireEvent, screen } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

vi.mock("@/lib/awsClient", () => ({
  getEksClient: () => ({ send: vi.fn() }),
}))

import { AccessTab } from "./access-tab"

const CLUSTER = "demo"
const PRINCIPAL = "arn:aws:iam::0:role/admin"

function seed() {
  const qc = createTestQueryClient()
  qc.setQueryData(["eks", "clusters", CLUSTER, "access-entries"], {
    accessEntries: [PRINCIPAL],
  })
  qc.setQueryData(["eks", "clusters", CLUSTER, "access-entries", PRINCIPAL], {
    accessEntry: { type: "STANDARD", kubernetesGroups: ["system:masters"] },
  })
  qc.setQueryData(
    ["eks", "clusters", CLUSTER, "access-entries", PRINCIPAL, "policies"],
    {
      associatedAccessPolicies: [
        {
          policyArn:
            "arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy",
        },
      ],
    },
  )
  return qc
}

describe("AccessTab", () => {
  it("renders an access entry with its policies", () => {
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    expect(screen.getByText(PRINCIPAL)).toBeInTheDocument()
    expect(screen.getByText("STANDARD")).toBeInTheDocument()
    expect(screen.getByText("system:masters")).toBeInTheDocument()
    expect(screen.getAllByText("AmazonEKSAdminPolicy").length).toBeGreaterThan(
      0,
    )
    expect(screen.getByRole("button", { name: "Remove" })).toBeInTheDocument()
  })

  it("shows empty state when there are no access entries", () => {
    const qc = createTestQueryClient()
    qc.setQueryData(["eks", "clusters", CLUSTER, "access-entries"], {
      accessEntries: [],
    })
    renderWithClient(<AccessTab clusterName={CLUSTER} />, qc)
    expect(screen.getByText("No access entries.")).toBeInTheDocument()
  })

  it("opens the add access entry dialog", () => {
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Add access entry" }))
    expect(screen.getByLabelText("IAM principal ARN")).toBeInTheDocument()
    expect(screen.getByLabelText("Kubernetes groups")).toBeInTheDocument()
  })

  it("opens the delete confirmation for an access entry", () => {
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    const deleteButton = screen
      .getAllByRole("button")
      .find((b) => b.textContent === "")
    expect(deleteButton).toBeDefined()
    fireEvent.click(deleteButton!)
    expect(
      screen.getByText(`Delete access entry for "${PRINCIPAL}"?`),
    ).toBeInTheDocument()
  })
})
