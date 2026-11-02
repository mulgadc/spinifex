import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { ReactNode } from "react"
import { afterEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() => vi.fn())

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { ListenerRulesTab } from "./listener-rules-tab"

const LISTENER_ARN = "arn:aws:elasticloadbalancing:listener/app/foo/abc/def"
const RULE_ARN = "arn:aws:elasticloadbalancing:listener-rule/app/foo/abc/def/1"
const TG_ARN = "arn:aws:elasticloadbalancing:tg/app/a/aaa"

const TARGET_GROUPS: TargetGroup[] = [
  { TargetGroupArn: TG_ARN, TargetGroupName: "tg-a", Protocol: "HTTP" },
]

function renderTab(rules: unknown[] = []) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        staleTime: Number.POSITIVE_INFINITY,
        refetchOnMount: false,
      },
      mutations: { retry: false },
    },
  })

  queryClient.setQueryData(["elbv2", "listeners", LISTENER_ARN, "rules"], {
    Rules: rules,
  })

  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    )
  }

  render(
    <ListenerRulesTab
      listenerArn={LISTENER_ARN}
      targetGroups={TARGET_GROUPS}
    />,
    { wrapper: Wrapper },
  )
}

describe("ListenerRulesTab", () => {
  afterEach(() => {
    mockSend.mockReset()
  })

  it("adds a path rule and sends CreateRule with the AWS condition shape", async () => {
    mockSend.mockResolvedValue({})
    const user = userEvent.setup()
    renderTab()

    await user.click(screen.getByRole("button", { name: "Add rule" }))
    const panel = await screen.findByRole("region", { name: "Add rule" })
    await user.type(within(panel).getByLabelText(/^Values/), "/api/*")
    await user.click(within(panel).getByLabelText("Forward to target group"))
    await user.click(await screen.findByText(/tg-a/))
    await user.click(within(panel).getByRole("button", { name: "Add rule" }))

    await waitFor(() => {
      expect(mockSend).toHaveBeenCalled()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      ListenerArn: LISTENER_ARN,
      Priority: 1,
      Conditions: [
        { Field: "path-pattern", PathPatternConfig: { Values: ["/api/*"] } },
      ],
      Actions: [{ Type: "forward", TargetGroupArn: TG_ARN }],
    })
  })

  it("keeps the form open with the refusal beside the button", async () => {
    mockSend.mockRejectedValue(
      Object.assign(new Error("Priority 1 is in use"), {
        name: "PriorityInUse",
      }),
    )
    const user = userEvent.setup()
    renderTab()

    await user.click(screen.getByRole("button", { name: "Add rule" }))
    const panel = await screen.findByRole("region", { name: "Add rule" })
    await user.type(within(panel).getByLabelText(/^Values/), "/api/*")
    await user.click(within(panel).getByLabelText("Forward to target group"))
    await user.click(await screen.findByText(/tg-a/))
    await user.click(within(panel).getByRole("button", { name: "Add rule" }))

    await expect(
      within(panel).findByText(/PriorityInUse: Priority 1 is in use/),
    ).resolves.toBeInTheDocument()
    expect(screen.getByRole("region", { name: "Add rule" })).toBeInTheDocument()
  })

  it("edits a rule and shows modify-rule in the CLI panel", async () => {
    const user = userEvent.setup()
    renderTab([
      {
        RuleArn: RULE_ARN,
        Priority: "10",
        IsDefault: false,
        Conditions: [{ Field: "host-header", Values: ["api.example.com"] }],
        Actions: [{ Type: "forward", TargetGroupArn: TG_ARN }],
      },
    ])

    await user.click(screen.getByRole("button", { name: /^Edit rule/ }))
    const panel = await screen.findByRole("region", { name: "Edit rule" })
    expect(within(panel).getByLabelText(/^Values/)).toHaveValue(
      "api.example.com",
    )
    await user.click(within(panel).getByRole("button", { name: "AWS CLI" }))
    expect(within(panel).getByText(/elbv2 modify-rule/)).toBeInTheDocument()
    expect(within(panel).getByText(RULE_ARN)).toBeInTheDocument()
  })
})
