import type { QueryClient } from "@tanstack/react-query"
import { fireEvent, screen, waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { send } = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock("@/lib/awsClient", () => ({
  getIamClient: () => ({ send }),
}))

import { AttachedPoliciesPanel } from "./attached-policies-panel"

const NAME = "alice"
const ATTACHED_ARN = "arn:aws:iam::aws:policy/AttachedPolicy"
const AVAILABLE_ARN = "arn:aws:iam::aws:policy/AvailablePolicy"

const ATTACHED_KEY = {
  user: "attached-user-policies",
  role: "attached-role-policies",
  group: "attached-group-policies",
} as const

function seed(
  kind: keyof typeof ATTACHED_KEY,
  attachedArns: string[],
): QueryClient {
  const qc = createTestQueryClient()
  qc.setQueryData(["iam", ATTACHED_KEY[kind], NAME], {
    AttachedPolicies: attachedArns.map((arn) => ({
      PolicyArn: arn,
      PolicyName: "AttachedPolicy",
    })),
  })
  qc.setQueryData(["iam", "policies"], {
    Policies: [
      { Arn: ATTACHED_ARN, PolicyName: "AttachedPolicy" },
      { Arn: AVAILABLE_ARN, PolicyName: "AvailablePolicy" },
    ],
  })
  return qc
}

describe("AttachedPoliciesPanel", () => {
  it("lists the attached policies", () => {
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", [ATTACHED_ARN]),
    )
    expect(screen.getByText(ATTACHED_ARN)).toBeInTheDocument()
  })

  it("shows an empty state with no attached policies", () => {
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", []),
    )
    expect(screen.getByText("No attached policies.")).toBeInTheDocument()
  })

  it("offers only the policies that are not already attached", () => {
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", [ATTACHED_ARN]),
    )
    fireEvent.click(screen.getByRole("button", { name: "Attach Policy" }))

    expect(
      screen.getByRole("button", { name: /AvailablePolicy/ }),
    ).toBeInTheDocument()
    expect(
      screen.queryByRole("button", { name: /^AttachedPolicy/ }),
    ).not.toBeInTheDocument()
  })

  it("reports when every policy is already attached", () => {
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", [ATTACHED_ARN, AVAILABLE_ARN]),
    )
    fireEvent.click(screen.getByRole("button", { name: "Attach Policy" }))

    expect(
      screen.getByText("No policies available to attach."),
    ).toBeInTheDocument()
  })

  it("attaches a policy via AttachUserPolicy and closes the select", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", []),
    )
    fireEvent.click(screen.getByRole("button", { name: "Attach Policy" }))
    fireEvent.click(screen.getByRole("button", { name: /AvailablePolicy/ }))

    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      UserName: NAME,
      PolicyArn: AVAILABLE_ARN,
    })
    await waitFor(() =>
      expect(
        screen.queryByText("Select a policy to attach:"),
      ).not.toBeInTheDocument(),
    )
  })

  it("detaches a policy via DetachUserPolicy", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", [ATTACHED_ARN]),
    )
    fireEvent.click(screen.getByRole("button", { name: "Detach" }))

    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      UserName: NAME,
      PolicyArn: ATTACHED_ARN,
    })
  })

  // A failed attach used to reject unhandled and collapse the select regardless.
  it("surfaces a failed attach and leaves the select open to retry", async () => {
    send.mockRejectedValue(new Error("Boom"))
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", []),
    )
    fireEvent.click(screen.getByRole("button", { name: "Attach Policy" }))
    fireEvent.click(screen.getByRole("button", { name: /AvailablePolicy/ }))

    expect(
      await screen.findByText("Failed to attach policy"),
    ).toBeInTheDocument()
    expect(screen.getByText("Select a policy to attach:")).toBeInTheDocument()
    expect(
      screen.getByRole("button", { name: /AvailablePolicy/ }),
    ).toBeEnabled()
  })

  it("surfaces a failed detach", async () => {
    send.mockRejectedValue(new Error("Boom"))
    renderWithClient(
      <AttachedPoliciesPanel kind="user" name={NAME} />,
      seed("user", [ATTACHED_ARN]),
    )
    fireEvent.click(screen.getByRole("button", { name: "Detach" }))

    expect(
      await screen.findByText("Failed to detach policy"),
    ).toBeInTheDocument()
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Detach" })).toBeEnabled(),
    )
  })

  it("attaches via AttachRolePolicy for a role", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <AttachedPoliciesPanel kind="role" name={NAME} />,
      seed("role", []),
    )
    fireEvent.click(screen.getByRole("button", { name: "Attach Policy" }))
    fireEvent.click(screen.getByRole("button", { name: /AvailablePolicy/ }))

    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      RoleName: NAME,
      PolicyArn: AVAILABLE_ARN,
    })
  })

  it("detaches via DetachGroupPolicy for a group", async () => {
    send.mockResolvedValue({})
    renderWithClient(
      <AttachedPoliciesPanel kind="group" name={NAME} />,
      seed("group", [ATTACHED_ARN]),
    )
    fireEvent.click(screen.getByRole("button", { name: "Detach" }))

    await waitFor(() => expect(send).toHaveBeenCalled())
    expect(send.mock.calls[0]![0].input).toStrictEqual({
      GroupName: NAME,
      PolicyArn: ATTACHED_ARN,
    })
  })
})
