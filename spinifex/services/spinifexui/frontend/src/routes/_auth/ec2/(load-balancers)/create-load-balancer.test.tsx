import type { Image, SecurityGroup, Subnet, Vpc } from "@aws-sdk/client-ec2"
import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  createTestQueryClient,
  renderWithClient,
} from "@/test/elbv2-integration"

const { routerState, sdk } = vi.hoisted(() => {
  interface Command {
    readonly constructor: { name: string }
    readonly input: unknown
  }
  const handlers = new Map<string, (input: unknown) => unknown>()
  const send = vi.fn(async (command: Command): Promise<unknown> => {
    const handler = handlers.get(command.constructor.name)
    if (!handler) {
      throw new Error(
        `No handler registered for SDK command ${command.constructor.name}`,
      )
    }
    return handler(command.input)
  })
  return {
    routerState: {
      navigate: vi.fn(),
      params: {} as Record<string, string>,
    },
    sdk: {
      send,
      setHandler: (name: string, handler: (input: unknown) => unknown) => {
        handlers.set(name, handler)
      },
      reset: () => {
        handlers.clear()
        send.mockClear()
      },
    },
  }
})

vi.mock("@/lib/awsClient", () => ({
  getElbv2Client: () => ({ send: sdk.send }),
  getEc2Client: () => ({ send: sdk.send }),
}))

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>()
  return {
    ...actual,
    createFileRoute: () => (options: Record<string, unknown>) => ({
      ...options,
      useParams: () => routerState.params,
    }),
    useNavigate: () => routerState.navigate,
    Link: ({ children, to }: { children: React.ReactNode; to?: string }) => (
      <a href={to}>{children}</a>
    ),
  }
})

import { CreateLoadBalancerPage } from "./-components/create-load-balancer-page"

const VPC_ID = "vpc-aaa"
const VPCS: Vpc[] = [{ VpcId: VPC_ID, CidrBlock: "10.0.0.0/16", Tags: [] }]
const SUBNETS: Subnet[] = [
  {
    SubnetId: "subnet-a",
    VpcId: VPC_ID,
    AvailabilityZone: "az-1",
    CidrBlock: "10.0.1.0/24",
  },
  {
    SubnetId: "subnet-b",
    VpcId: VPC_ID,
    AvailabilityZone: "az-2",
    CidrBlock: "10.0.2.0/24",
  },
]
const SGS: SecurityGroup[] = [
  { GroupId: "sg-1", GroupName: "web", VpcId: VPC_ID },
]
const EXISTING_TG: TargetGroup = {
  TargetGroupArn: "arn:tg:existing",
  TargetGroupName: "existing-tg",
  Protocol: "HTTP",
  Port: 80,
  VpcId: VPC_ID,
}
const LB_IMAGE: Image = {
  ImageId: "ami-lb",
  Name: "lb-alpine-3.21.6-x86_64",
  Tags: [{ Key: "spinifex:managed-by", Value: "elbv2" }],
}

function seedClient(options: { lbImageImported?: boolean } = {}) {
  const { lbImageImported = true } = options
  const qc = createTestQueryClient()
  qc.setQueryData(["ec2", "vpcs"], { Vpcs: VPCS })
  qc.setQueryData(["ec2", "subnets"], { Subnets: SUBNETS })
  qc.setQueryData(["ec2", "securityGroups"], { SecurityGroups: SGS })
  qc.setQueryData(["elbv2", "targetGroups"], { TargetGroups: [EXISTING_TG] })
  qc.setQueryData(["ec2", "images"], {
    Images: lbImageImported ? [LB_IMAGE] : [],
  })
  return qc
}

async function selectSubnets(user: ReturnType<typeof userEvent.setup>) {
  const checkboxes = screen.getAllByRole("checkbox")
  const [first, second] = checkboxes
  if (!first || !second) {
    throw new Error("expected at least two subnet checkboxes to be rendered")
  }
  await user.click(first)
  await user.click(second)
}

describe("create-load-balancer route", () => {
  beforeEach(() => {
    sdk.reset()
    routerState.navigate.mockClear()
    // Wizard's onSettled invalidates ["elbv2"] which triggers a refetch of
    // Describe queries — stub them so those SDK calls don't reject.
    sdk.setHandler("DescribeLoadBalancersCommand", () => ({
      LoadBalancers: [],
    }))
    sdk.setHandler("DescribeTargetGroupsCommand", () => ({ TargetGroups: [] }))
  })
  afterEach(() => vi.clearAllMocks())

  it("wizard happy path with a new target group creates TG → LB → listener and navigates", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateTargetGroupCommand", () => ({
      TargetGroups: [{ TargetGroupArn: "arn:tg:new" }],
    }))
    sdk.setHandler("CreateLoadBalancerCommand", () => ({
      LoadBalancers: [{ LoadBalancerArn: "arn:lb:new" }],
    }))
    sdk.setHandler("CreateListenerCommand", () => ({
      Listeners: [{ ListenerArn: "arn:listener:new" }],
    }))

    renderWithClient(<CreateLoadBalancerPage />, seedClient())

    await user.type(
      screen.getByLabelText("Name", { selector: "#lb-name" }),
      "my-alb",
    )
    await user.type(
      screen.getByLabelText("Name", { selector: "#tg-name" }),
      "my-tg",
    )
    await selectSubnets(user)

    await user.click(
      screen.getByRole("button", { name: "Create load balancer" }),
    )

    await waitFor(() => {
      expect(routerState.navigate).toHaveBeenCalled()
    })
    const createCommands = sdk.send.mock.calls
      .map(
        (call) =>
          (call[0] as { constructor: { name: string } }).constructor.name,
      )
      .filter((name) => name.startsWith("Create"))
    expect(createCommands).toEqual([
      "CreateTargetGroupCommand",
      "CreateLoadBalancerCommand",
      "CreateListenerCommand",
    ])

    await waitFor(() => {
      expect(routerState.navigate).toHaveBeenCalledWith({
        to: "/ec2/describe-load-balancers/$id",
        params: { id: encodeURIComponent("arn:lb:new") },
      })
    })
  })

  it("wizard happy path with existing target group skips TG creation", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateLoadBalancerCommand", () => ({
      LoadBalancers: [{ LoadBalancerArn: "arn:lb:new" }],
    }))
    sdk.setHandler("CreateListenerCommand", () => ({}))

    renderWithClient(<CreateLoadBalancerPage />, seedClient())

    await user.type(
      screen.getByLabelText("Name", { selector: "#lb-name" }),
      "my-alb",
    )
    await selectSubnets(user)

    // Switch to existing TG mode and select it
    const existingRadio = screen.getByLabelText("Use existing")
    await user.click(existingRadio)
    // Base UI Select — click trigger to open, then the option
    const tgTrigger = await screen.findByLabelText("Target group")
    await user.click(tgTrigger)
    const tgItem = await screen.findByText(/existing-tg · HTTP:80/)
    await user.click(tgItem)

    await user.click(
      screen.getByRole("button", { name: "Create load balancer" }),
    )

    await waitFor(() => {
      expect(routerState.navigate).toHaveBeenCalled()
    })
    const createCalls = sdk.send.mock.calls.filter((call) =>
      (
        call[0] as { constructor: { name: string } }
      ).constructor.name.startsWith("Create"),
    )
    const createNames = createCalls.map(
      (call) => (call[0] as { constructor: { name: string } }).constructor.name,
    )
    expect(createNames).toEqual([
      "CreateLoadBalancerCommand",
      "CreateListenerCommand",
    ])
    const listenerInput = createCalls[1]?.[0].input as {
      DefaultActions: { Type: string; TargetGroupArn: string }[]
    }
    expect(listenerInput.DefaultActions).toEqual([
      { Type: "forward", TargetGroupArn: "arn:tg:existing" },
    ])
  })

  it("surfaces wizard failure with partial cleanup guidance when LB step fails", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateTargetGroupCommand", () => ({
      TargetGroups: [{ TargetGroupArn: "arn:tg:new" }],
    }))
    sdk.setHandler("CreateLoadBalancerCommand", () => {
      throw new Error("subnets span must be ≥2 AZs")
    })

    renderWithClient(<CreateLoadBalancerPage />, seedClient())

    await user.type(
      screen.getByLabelText("Name", { selector: "#lb-name" }),
      "my-alb",
    )
    await user.type(
      screen.getByLabelText("Name", { selector: "#tg-name" }),
      "my-tg",
    )
    await selectSubnets(user)

    await user.click(
      screen.getByRole("button", { name: "Create load balancer" }),
    )

    expect(
      await screen.findByText(/Wizard failed: creating load balancer/i),
    ).toBeInTheDocument()
    expect(screen.getByText(/subnets span must be ≥2 AZs/)).toBeInTheDocument()
    // Partial-cleanup list should mention the orphaned TG
    expect(screen.getByText(/Target Group:/)).toBeInTheDocument()
    expect(screen.getByText(/arn:tg:new/)).toBeInTheDocument()
    expect(routerState.navigate).not.toHaveBeenCalled()
  })

  it("blocks form render with import banner when LB AMI missing", () => {
    renderWithClient(
      <CreateLoadBalancerPage />,
      seedClient({ lbImageImported: false }),
    )

    expect(
      screen.getByText("Load balancer image not imported"),
    ).toBeInTheDocument()
    expect(
      screen.getByText(/spx admin images import --name lb-alpine/),
    ).toBeInTheDocument()
    // Form inputs must not render
    expect(
      screen.queryByLabelText("Name", { selector: "#lb-name" }),
    ).not.toBeInTheDocument()
  })
})
