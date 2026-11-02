import { fireEvent, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderWithClient } from "@/test/render"

const send = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(send)
})

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

  it("opens the add access entry form in a panel", () => {
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Add access entry" }))
    const panel = screen.getByRole("region", { name: "Add access entry" })
    expect(
      within(panel).getByLabelText("IAM principal ARN"),
    ).toBeInTheDocument()
    expect(
      within(panel).getByLabelText("Kubernetes groups"),
    ).toBeInTheDocument()
  })

  it("creates an access entry with split groups and closes the panel", async () => {
    send.mockResolvedValueOnce({})
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Add access entry" }))
    const panel = screen.getByRole("region", { name: "Add access entry" })
    fireEvent.change(within(panel).getByLabelText("IAM principal ARN"), {
      target: { value: PRINCIPAL },
    })
    fireEvent.change(within(panel).getByLabelText("Kubernetes groups"), {
      target: { value: "devs, ops" },
    })
    fireEvent.click(within(panel).getByRole("button", { name: "AWS CLI" }))
    expect(panel).toHaveTextContent("--kubernetes-groups 'devs' 'ops'")
    fireEvent.click(within(panel).getByRole("button", { name: "Create" }))

    await waitFor(() => {
      expect(
        screen.queryByRole("region", { name: "Add access entry" }),
      ).not.toBeInTheDocument()
    })
    expect(send.mock.calls[0]?.[0].input).toMatchObject({
      clusterName: CLUSTER,
      principalArn: PRINCIPAL,
      kubernetesGroups: ["devs", "ops"],
    })
  })

  it("associates the access policy chosen in the row", async () => {
    send.mockResolvedValue({})
    const user = userEvent.setup()
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    await user.click(screen.getByRole("combobox", { name: "Access policy" }))
    await user.click(
      screen.getByRole("option", { name: "AmazonEKSAdminPolicy" }),
    )
    await user.click(screen.getByRole("button", { name: "Associate" }))

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith(
        expect.objectContaining({
          input: expect.objectContaining({
            principalArn: PRINCIPAL,
            policyArn:
              "arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy",
          }),
        }),
      )
    })
    send.mockReset()
  })

  it("opens the delete confirmation for an access entry", () => {
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Delete access entry" }))
    expect(
      screen.getByText(`Delete access entry for "${PRINCIPAL}"?`),
    ).toBeInTheDocument()
  })

  it("shows a refused create in the panel and keeps it open", async () => {
    send.mockRejectedValueOnce(
      Object.assign(new Error("The access entry already exists."), {
        name: "ResourceInUseException",
      }),
    )
    renderWithClient(<AccessTab clusterName={CLUSTER} />, seed())
    fireEvent.click(screen.getByRole("button", { name: "Add access entry" }))
    const dialog = screen.getByRole("region", { name: "Add access entry" })
    fireEvent.change(within(dialog).getByLabelText("IAM principal ARN"), {
      target: { value: PRINCIPAL },
    })
    fireEvent.click(within(dialog).getByRole("button", { name: "Create" }))

    await expect(
      within(dialog).findByText(
        "ResourceInUseException: The access entry already exists.",
      ),
    ).resolves.toBeInTheDocument()
    expect(send.mock.calls[0]?.[0].input.principalArn).toBe(PRINCIPAL)
    expect(screen.getByLabelText("IAM principal ARN")).toHaveValue(PRINCIPAL)
    expect(within(dialog).getByRole("button", { name: "Create" })).toBeEnabled()
  })
})
