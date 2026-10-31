import type { SecurityGroup, Subnet, Vpc } from "@aws-sdk/client-ec2"
import type {
  CreateLoadBalancerCommandInput,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { acmCertificatesQueryOptions } from "@/queries/acm"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  elbv2SslPoliciesQueryOptions,
  elbv2TargetGroupsQueryOptions,
} from "@/queries/elbv2"
import { createTestQueryClient, renderWithClient } from "@/test/render"

const sdk = await vi.hoisted(async () => {
  const { createFakeSdk } = await import("@/test/fake-sdk")

  return createFakeSdk()
})

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(sdk.send)
})

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const { mockRouter } = await import("@/test/router")

  return await mockRouter(importOriginal)
})

import { CreateLoadBalancerPage } from "./create-load-balancer-page"

const onClose = vi.fn()

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

function seedClient() {
  const qc = createTestQueryClient()
  const $metadata = {}
  qc.setQueryData(ec2VpcsQueryOptions.queryKey, { Vpcs: VPCS, $metadata })
  qc.setQueryData(ec2SubnetsQueryOptions.queryKey, {
    Subnets: SUBNETS,
    $metadata,
  })
  qc.setQueryData(ec2SecurityGroupsQueryOptions.queryKey, {
    SecurityGroups: SGS,
    $metadata,
  })
  qc.setQueryData(elbv2TargetGroupsQueryOptions.queryKey, {
    TargetGroups: [EXISTING_TG],
    $metadata,
  })
  qc.setQueryData(acmCertificatesQueryOptions.queryKey, {
    CertificateSummaryList: [],
    $metadata,
  })
  qc.setQueryData(elbv2SslPoliciesQueryOptions.queryKey, {
    SslPolicies: [],
    $metadata,
  })

  return qc
}

async function selectSubnets(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("switch", { name: "Subnet subnet-a" }))
  await user.click(screen.getByRole("switch", { name: "Subnet subnet-b" }))
}

describe("CreateLoadBalancerPage", () => {
  beforeEach(() => {
    sdk.reset()
    onClose.mockReset()
    // Wizard's onSettled invalidates ["elbv2"] which triggers a refetch of
    // Describe queries — stub them so those SDK calls don't reject.
    sdk.setHandler("DescribeLoadBalancersCommand", () => ({
      LoadBalancers: [],
    }))
    sdk.setHandler("DescribeTargetGroupsCommand", () => ({ TargetGroups: [] }))
  })

  it("wizard happy path with a new target group creates TG → LB → listener and closes with its ARN", async () => {
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

    renderWithClient(<CreateLoadBalancerPage onClose={onClose} />, seedClient())

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
      screen.getByRole("button", { name: "Create Load Balancer" }),
    )

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })

    const createCommands = sdk.send.mock.calls
      .map(
        (call) =>
          (call[0] as { constructor: { name: string } }).constructor.name,
      )
      .filter((name) => name.startsWith("Create"))

    expect(createCommands).toStrictEqual([
      "CreateTargetGroupCommand",
      "CreateLoadBalancerCommand",
      "CreateListenerCommand",
    ])

    await waitFor(() => {
      expect(onClose).toHaveBeenCalledWith("arn:lb:new")
    })
  })

  it("sends the chosen scheme and security groups", async () => {
    const user = userEvent.setup()

    const createLb = vi.fn((_input: CreateLoadBalancerCommandInput) => ({
      LoadBalancers: [{ LoadBalancerArn: "arn:lb:new" }],
    }))

    sdk.setHandler("CreateTargetGroupCommand", () => ({
      TargetGroups: [{ TargetGroupArn: "arn:tg:new" }],
    }))
    sdk.setHandler("CreateLoadBalancerCommand", createLb)
    sdk.setHandler("CreateListenerCommand", () => ({ Listeners: [] }))

    renderWithClient(<CreateLoadBalancerPage onClose={onClose} />, seedClient())

    await user.type(
      screen.getByLabelText("Name", { selector: "#lb-name" }),
      "my-alb",
    )
    await user.type(
      screen.getByLabelText("Name", { selector: "#tg-name" }),
      "my-tg",
    )
    await selectSubnets(user)
    await user.click(screen.getByLabelText("Internal"))
    await user.click(screen.getByLabelText("Security group sg-1 (web)"))

    await user.click(
      screen.getByRole("button", { name: "Create Load Balancer" }),
    )

    await waitFor(() => {
      expect(createLb).toHaveBeenCalledWith(
        expect.objectContaining({
          Scheme: "internal",
          SecurityGroups: ["sg-1"],
        }),
      )
    })
  })

  it("wizard happy path with existing target group skips TG creation", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateLoadBalancerCommand", () => ({
      LoadBalancers: [{ LoadBalancerArn: "arn:lb:new" }],
    }))
    sdk.setHandler("CreateListenerCommand", () => ({}))

    renderWithClient(<CreateLoadBalancerPage onClose={onClose} />, seedClient())

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
      screen.getByRole("button", { name: "Create Load Balancer" }),
    )

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })

    const createCalls = sdk.send.mock.calls.filter((call) =>
      (
        call[0] as { constructor: { name: string } }
      ).constructor.name.startsWith("Create"),
    )

    const createNames = createCalls.map(
      (call) => (call[0] as { constructor: { name: string } }).constructor.name,
    )

    expect(createNames).toStrictEqual([
      "CreateLoadBalancerCommand",
      "CreateListenerCommand",
    ])

    const listenerInput = createCalls[1]?.[0].input as {
      DefaultActions: { Type: string; TargetGroupArn: string }[]
    }

    expect(listenerInput.DefaultActions).toStrictEqual([
      { Type: "forward", TargetGroupArn: "arn:tg:existing" },
    ])
  })

  it("selecting Network creates an NLB with a TCP listener and target group", async () => {
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

    renderWithClient(<CreateLoadBalancerPage onClose={onClose} />, seedClient())

    await user.click(screen.getByLabelText("Network Load Balancer"))

    await user.type(
      screen.getByLabelText("Name", { selector: "#lb-name" }),
      "my-nlb",
    )
    await user.type(
      screen.getByLabelText("Name", { selector: "#tg-name" }),
      "my-tg",
    )
    await selectSubnets(user)

    await user.click(
      screen.getByRole("button", { name: "Create Load Balancer" }),
    )

    await waitFor(() => {
      expect(onClose).toHaveBeenCalled()
    })

    const createCalls = sdk.send.mock.calls.filter((call) =>
      (
        call[0] as { constructor: { name: string } }
      ).constructor.name.startsWith("Create"),
    )

    const byName = (name: string) =>
      createCalls.find(
        (call) =>
          (call[0] as { constructor: { name: string } }).constructor.name ===
          name,
        // oxlint-disable-next-line anti-slop/no-unsafe-dictionary-type -- the call log holds inputs of several command types; the assertions below name the fields
      )?.[0].input as Record<string, unknown> | undefined

    expect(byName("CreateLoadBalancerCommand")).toMatchObject({
      Type: "network",
    })
    // NLB hides the security-group field, so none are submitted.
    expect(byName("CreateLoadBalancerCommand")?.SecurityGroups).toBeUndefined()
    expect(byName("CreateTargetGroupCommand")).toMatchObject({
      Protocol: "TCP",
      HealthCheckProtocol: "TCP",
    })
    // TCP health checks have neither an HTTP path nor a matcher.
    expect(byName("CreateTargetGroupCommand")?.Matcher).toBeUndefined()
    expect(byName("CreateTargetGroupCommand")?.HealthCheckPath).toBeUndefined()
    expect(byName("CreateListenerCommand")).toMatchObject({
      Protocol: "TCP",
      Port: 80,
    })
  })

  it("surfaces wizard failure with partial cleanup guidance when LB step fails", async () => {
    const user = userEvent.setup()
    sdk.setHandler("CreateTargetGroupCommand", () => ({
      TargetGroups: [{ TargetGroupArn: "arn:tg:new" }],
    }))
    sdk.setHandler("CreateLoadBalancerCommand", () => {
      throw new Error("subnets span must be ≥2 AZs")
    })

    renderWithClient(<CreateLoadBalancerPage onClose={onClose} />, seedClient())

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
      screen.getByRole("button", { name: "Create Load Balancer" }),
    )

    await expect(
      screen.findByText(/Wizard failed: creating load balancer/i),
    ).resolves.toBeInTheDocument()
    expect(screen.getByText(/subnets span must be ≥2 AZs/)).toBeInTheDocument()
    // Partial-cleanup list should mention the orphaned TG
    expect(screen.getByText(/Target Group:/)).toBeInTheDocument()
    expect(screen.getByText(/arn:tg:new/)).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
  })
})
