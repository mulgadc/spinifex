import { describe, expect, it } from "vitest"

import {
  createListenerSchema,
  createLoadBalancerSchema,
  createTargetGroupSchema,
  healthCheckSchema,
  registerTargetsSchema,
} from "./elbv2"

const defaultHealthCheck = {
  protocol: "HTTP" as const,
  path: "/",
  port: "traffic-port",
  intervalSeconds: 30,
  timeoutSeconds: 5,
  healthyThresholdCount: 5,
  unhealthyThresholdCount: 2,
  matcher: "200",
}

describe("createTargetGroupSchema", () => {
  it("accepts a valid HTTP target group", () => {
    const result = createTargetGroupSchema.safeParse({
      name: "my-tg",
      protocol: "HTTP",
      port: 80,
      vpcId: "vpc-123",
      healthCheck: defaultHealthCheck,
      tags: [],
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects name with invalid characters", () => {
    const result = createTargetGroupSchema.safeParse({
      name: "my_tg",
      protocol: "HTTP",
      port: 80,
      vpcId: "vpc-123",
      healthCheck: defaultHealthCheck,
      tags: [],
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects name >32 chars", () => {
    const result = createTargetGroupSchema.safeParse({
      name: "x".repeat(33),
      protocol: "HTTP",
      port: 80,
      vpcId: "vpc-123",
      healthCheck: defaultHealthCheck,
      tags: [],
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects port out of range", () => {
    const result = createTargetGroupSchema.safeParse({
      name: "my-tg",
      protocol: "HTTP",
      port: 70_000,
      vpcId: "vpc-123",
      healthCheck: defaultHealthCheck,
      tags: [],
    })
    expect(result.success).toBeFalsy()
  })
})

describe("createLoadBalancerSchema", () => {
  const baseListener = {
    protocol: "HTTP" as const,
    port: 80,
    targetGroupMode: "existing" as const,
    existingTargetGroupArn: "arn:tg:1",
  }

  it("accepts a valid ALB with 2+ subnets", () => {
    const result = createLoadBalancerSchema.safeParse({
      name: "my-alb",
      type: "application",
      scheme: "internet-facing",
      vpcId: "vpc-1",
      subnetIds: ["subnet-a", "subnet-b"],
      securityGroupIds: ["sg-1"],
      tags: [],
      listener: baseListener,
    })
    expect(result.success).toBeTruthy()
  })

  it("accepts a single subnet", () => {
    const result = createLoadBalancerSchema.safeParse({
      name: "my-alb",
      type: "application",
      scheme: "internet-facing",
      vpcId: "vpc-1",
      subnetIds: ["subnet-a"],
      securityGroupIds: [],
      tags: [],
      listener: baseListener,
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects zero subnets", () => {
    const result = createLoadBalancerSchema.safeParse({
      name: "my-alb",
      type: "application",
      scheme: "internet-facing",
      vpcId: "vpc-1",
      subnetIds: [],
      securityGroupIds: [],
      tags: [],
      listener: baseListener,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects names starting with 'internal-'", () => {
    const result = createLoadBalancerSchema.safeParse({
      name: "internal-abc",
      type: "application",
      scheme: "internal",
      vpcId: "vpc-1",
      subnetIds: ["subnet-a", "subnet-b"],
      securityGroupIds: [],
      tags: [],
      listener: baseListener,
    })
    expect(result.success).toBeFalsy()
  })

  it("accepts listener with mode=new without existingTargetGroupArn", () => {
    const result = createLoadBalancerSchema.safeParse({
      name: "my-alb",
      type: "application",
      scheme: "internet-facing",
      vpcId: "vpc-1",
      subnetIds: ["subnet-a", "subnet-b"],
      securityGroupIds: [],
      tags: [],
      listener: {
        protocol: "HTTP",
        port: 80,
        targetGroupMode: "new",
      },
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects listener with mode=existing but no existingTargetGroupArn", () => {
    const result = createLoadBalancerSchema.safeParse({
      name: "my-alb",
      type: "application",
      scheme: "internet-facing",
      vpcId: "vpc-1",
      subnetIds: ["subnet-a", "subnet-b"],
      securityGroupIds: [],
      tags: [],
      listener: {
        protocol: "HTTP",
        port: 80,
        targetGroupMode: "existing",
      },
    })
    expect(result.success).toBeFalsy()
  })
})

describe("createListenerSchema", () => {
  it("accepts a valid listener", () => {
    const result = createListenerSchema.safeParse({
      protocol: "HTTP",
      port: 80,
      defaultTargetGroupArn: "arn:tg:1",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects listener missing default target group", () => {
    const result = createListenerSchema.safeParse({
      protocol: "HTTP",
      port: 80,
      defaultTargetGroupArn: "",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("registerTargetsSchema", () => {
  it("accepts one or more targets", () => {
    const result = registerTargetsSchema.safeParse({
      targets: [{ instanceId: "i-123" }, { instanceId: "i-456", port: 8080 }],
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty target list", () => {
    const result = registerTargetsSchema.safeParse({ targets: [] })
    expect(result.success).toBeFalsy()
  })
})

describe("healthCheckSchema", () => {
  it("accepts the documented default values", () => {
    expect(healthCheckSchema.parse(defaultHealthCheck)).toStrictEqual(
      defaultHealthCheck,
    )
  })

  it("rejects an out-of-range interval", () => {
    const result = healthCheckSchema.safeParse({
      ...defaultHealthCheck,
      intervalSeconds: 1,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects a malformed matcher", () => {
    const result = healthCheckSchema.safeParse({
      ...defaultHealthCheck,
      matcher: "OK",
    })
    expect(result.success).toBeFalsy()
  })
})
