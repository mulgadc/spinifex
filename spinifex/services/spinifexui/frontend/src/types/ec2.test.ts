import { describe, expect, it } from "vitest"

import {
  attachVolumeSchema,
  copySnapshotSchema,
  createInstanceSchema,
  createKeyPairSchema,
  createPlacementGroupSchema,
  createSecurityGroupSchema,
  createSnapshotSchema,
  createSubnetSchema,
  createVolumeSchema,
  createVpcSchema,
  createVpcWizardSchema,
  formTagSchema,
  importKeyPairSchema,
  modifyVolumeSchema,
  securityGroupRuleSchema,
} from "./ec2"

describe("createInstanceSchema", () => {
  it("accepts valid instance params", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
    })
    expect(result.success).toBeTruthy()
  })

  it("requires count to be at least 1", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 0,
    })
    expect(result.success).toBeFalsy()
  })

  it("requires count to be an integer", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1.5,
    })
    expect(result.success).toBeFalsy()
  })

  it("allows optional subnetId", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      subnetId: "subnet-abc",
    })
    expect(result.success).toBeTruthy()
  })

  it("allows optional placementGroupName", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      placementGroupName: "my-group",
    })
    expect(result.success).toBeTruthy()
  })

  it("supports capacity refine", () => {
    const refined = createInstanceSchema.refine((data) => data.count <= 3, {
      message: "Cannot exceed available capacity",
      path: ["count"],
    })
    const result = refined.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 5,
    })
    if (result.success) {
      throw new Error("expected validation to fail")
    }
    expect(result.error.issues[0]?.message).toBe(
      "Cannot exceed available capacity",
    )
  })

  it("accepts optional root volume fields", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootDeviceName: "/dev/sda1",
      rootVolumeSize: 50,
      rootVolumeType: "gp3",
      rootDeleteOnTermination: true,
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects rootVolumeSize below 1 GiB", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootVolumeSize: 0,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects rootVolumeSize above 16384 GiB", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootVolumeSize: 16_385,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects non-integer rootVolumeSize", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootVolumeSize: 50.5,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects unsupported rootVolumeType", () => {
    const result = createInstanceSchema.safeParse({
      imageId: "ami-123",
      instanceType: "t2.micro",
      keyName: "my-key",
      count: 1,
      rootVolumeType: "io2",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("createKeyPairSchema", () => {
  it("accepts a valid key name", () => {
    const result = createKeyPairSchema.safeParse({ keyName: "my-key" })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty key name", () => {
    const result = createKeyPairSchema.safeParse({ keyName: "" })
    expect(result.success).toBeFalsy()
  })

  it("rejects key name over 255 chars", () => {
    const result = createKeyPairSchema.safeParse({ keyName: "a".repeat(256) })
    expect(result.success).toBeFalsy()
  })
})

describe("importKeyPairSchema", () => {
  it("accepts valid key pair import", () => {
    const result = importKeyPairSchema.safeParse({
      keyName: "my-key",
      publicKeyMaterial: "ssh-rsa AAAAB3Nza...",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty public key", () => {
    const result = importKeyPairSchema.safeParse({
      keyName: "my-key",
      publicKeyMaterial: "",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects whitespace-only public key", () => {
    const result = importKeyPairSchema.safeParse({
      keyName: "my-key",
      publicKeyMaterial: "   ",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("createVolumeSchema", () => {
  it("accepts valid volume params", () => {
    const result = createVolumeSchema.safeParse({
      size: 10,
      availabilityZone: "us-east-1a",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects size below 1", () => {
    const result = createVolumeSchema.safeParse({
      size: 0,
      availabilityZone: "us-east-1a",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects size above 16384", () => {
    const result = createVolumeSchema.safeParse({
      size: 16_385,
      availabilityZone: "us-east-1a",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects fractional size", () => {
    const result = createVolumeSchema.safeParse({
      size: 10.5,
      availabilityZone: "us-east-1a",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("modifyVolumeSchema", () => {
  it("accepts valid size", () => {
    const result = modifyVolumeSchema.safeParse({ size: 20 })
    expect(result.success).toBeTruthy()
  })

  it("rejects size below 1", () => {
    const result = modifyVolumeSchema.safeParse({ size: 0 })
    expect(result.success).toBeFalsy()
  })
})

describe("createVpcSchema", () => {
  it("accepts valid CIDR block", () => {
    const result = createVpcSchema.safeParse({ cidrBlock: "10.0.0.0/16" })
    expect(result.success).toBeTruthy()
  })

  it("accepts CIDR block with optional name", () => {
    const result = createVpcSchema.safeParse({
      cidrBlock: "10.0.0.0/16",
      name: "my-vpc",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects invalid CIDR format", () => {
    const result = createVpcSchema.safeParse({ cidrBlock: "not-a-cidr" })
    expect(result.success).toBeFalsy()
  })

  it("rejects empty CIDR block", () => {
    const result = createVpcSchema.safeParse({ cidrBlock: "" })
    expect(result.success).toBeFalsy()
  })
})

describe("createSubnetSchema", () => {
  it("accepts valid subnet params", () => {
    const result = createSubnetSchema.safeParse({
      vpcId: "vpc-123",
      cidrBlock: "10.0.1.0/24",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects invalid CIDR block", () => {
    const result = createSubnetSchema.safeParse({
      vpcId: "vpc-123",
      cidrBlock: "invalid",
    })
    expect(result.success).toBeFalsy()
  })

  it("allows optional availability zone", () => {
    const result = createSubnetSchema.safeParse({
      vpcId: "vpc-123",
      cidrBlock: "10.0.1.0/24",
      availabilityZone: "us-east-1a",
    })
    expect(result.success).toBeTruthy()
  })
})

describe("createSnapshotSchema", () => {
  it("accepts valid snapshot params", () => {
    const result = createSnapshotSchema.safeParse({ volumeId: "vol-123" })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty volumeId", () => {
    const result = createSnapshotSchema.safeParse({ volumeId: "" })
    expect(result.success).toBeFalsy()
  })
})

describe("copySnapshotSchema", () => {
  it("accepts valid copy params", () => {
    const result = copySnapshotSchema.safeParse({
      sourceSnapshotId: "snap-123",
      sourceRegion: "us-east-1",
    })
    expect(result.success).toBeTruthy()
  })
})

describe("attachVolumeSchema", () => {
  it("accepts valid attach params", () => {
    const result = attachVolumeSchema.safeParse({
      volumeId: "vol-123",
      instanceId: "i-123",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty instanceId", () => {
    const result = attachVolumeSchema.safeParse({
      volumeId: "vol-123",
      instanceId: "",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("createPlacementGroupSchema", () => {
  it("accepts valid placement group params", () => {
    const result = createPlacementGroupSchema.safeParse({
      groupName: "my-group",
      strategy: "spread",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty group name", () => {
    const result = createPlacementGroupSchema.safeParse({
      groupName: "",
      strategy: "spread",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects group name over 255 chars", () => {
    const result = createPlacementGroupSchema.safeParse({
      groupName: "a".repeat(256),
      strategy: "cluster",
    })
    expect(result.success).toBeFalsy()
  })

  it("accepts group name at 255 chars", () => {
    const result = createPlacementGroupSchema.safeParse({
      groupName: "a".repeat(255),
      strategy: "spread",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty strategy", () => {
    const result = createPlacementGroupSchema.safeParse({
      groupName: "my-group",
      strategy: "",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects missing strategy", () => {
    const result = createPlacementGroupSchema.safeParse({
      groupName: "my-group",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects missing group name", () => {
    const result = createPlacementGroupSchema.safeParse({
      strategy: "spread",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("formTagSchema", () => {
  it("accepts valid tag with key and value", () => {
    const result = formTagSchema.safeParse({ key: "Env", value: "prod" })
    expect(result.success).toBeTruthy()
  })

  it("accepts tag with empty value", () => {
    const result = formTagSchema.safeParse({ key: "Env", value: "" })
    expect(result.success).toBeTruthy()
  })

  it("rejects tag with empty key", () => {
    const result = formTagSchema.safeParse({ key: "", value: "prod" })
    expect(result.success).toBeFalsy()
  })
})

describe("createSecurityGroupSchema", () => {
  it("accepts valid security group params", () => {
    const result = createSecurityGroupSchema.safeParse({
      groupName: "web-sg",
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty group name", () => {
    const result = createSecurityGroupSchema.safeParse({
      groupName: "",
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects group name over 255 chars", () => {
    const result = createSecurityGroupSchema.safeParse({
      groupName: "a".repeat(256),
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })
    expect(result.success).toBeFalsy()
  })

  it("accepts group name at 255 chars", () => {
    const result = createSecurityGroupSchema.safeParse({
      groupName: "a".repeat(255),
      description: "Allow web traffic",
      vpcId: "vpc-123",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty description", () => {
    const result = createSecurityGroupSchema.safeParse({
      groupName: "web-sg",
      description: "",
      vpcId: "vpc-123",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects description over 255 chars", () => {
    const result = createSecurityGroupSchema.safeParse({
      groupName: "web-sg",
      description: "a".repeat(256),
      vpcId: "vpc-123",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects empty vpcId", () => {
    const result = createSecurityGroupSchema.safeParse({
      groupName: "web-sg",
      description: "Allow web traffic",
      vpcId: "",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("securityGroupRuleSchema", () => {
  it("accepts valid TCP rule", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "tcp",
      fromPort: 443,
      toPort: 443,
      cidrIp: "0.0.0.0/0",
    })
    expect(result.success).toBeTruthy()
  })

  it("accepts all-traffic protocol with port -1", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "-1",
      fromPort: -1,
      toPort: -1,
      cidrIp: "10.0.0.0/16",
    })
    expect(result.success).toBeTruthy()
  })

  it("accepts port 65535 (upper boundary)", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "tcp",
      fromPort: 65_535,
      toPort: 65_535,
      cidrIp: "0.0.0.0/0",
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects port 65536 (above boundary)", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "tcp",
      fromPort: 65_536,
      toPort: 443,
      cidrIp: "0.0.0.0/0",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects port -2 (below boundary)", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "tcp",
      fromPort: -2,
      toPort: 443,
      cidrIp: "0.0.0.0/0",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects fractional port", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "tcp",
      fromPort: 22.5,
      toPort: 443,
      cidrIp: "0.0.0.0/0",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects invalid CIDR format", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "tcp",
      fromPort: 22,
      toPort: 22,
      cidrIp: "not-a-cidr",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects empty CIDR", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "tcp",
      fromPort: 22,
      toPort: 22,
      cidrIp: "",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects empty protocol", () => {
    const result = securityGroupRuleSchema.safeParse({
      ipProtocol: "",
      fromPort: 22,
      toPort: 22,
      cidrIp: "0.0.0.0/0",
    })
    expect(result.success).toBeFalsy()
  })
})

describe("createVpcWizardSchema", () => {
  const validBase = {
    mode: "vpc-only" as const,
    namePrefix: "test",
    autoGenerateNames: true,
    cidrBlock: "10.0.0.0/16",
    tenancy: "default" as const,
    publicSubnetCount: 0,
    privateSubnetCount: 0,
    publicSubnetCidrs: [],
    privateSubnetCidrs: [],
    tags: [],
  }

  it("accepts valid vpc-only mode with minimal fields", () => {
    const result = createVpcWizardSchema.safeParse(validBase)
    expect(result.success).toBeTruthy()
  })

  it("accepts valid vpc-and-more mode", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects invalid mode", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      mode: "invalid",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects empty CIDR block", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      cidrBlock: "",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects invalid CIDR format", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      cidrBlock: "not-a-cidr",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects CIDR with out-of-range octets", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      cidrBlock: "999.0.0.0/16",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects CIDR with prefix out of /16-/28 range", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      cidrBlock: "10.0.0.0/8",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects invalid tenancy", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      tenancy: "shared",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects publicSubnetCount above 4", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      publicSubnetCount: 5,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects negative privateSubnetCount", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      privateSubnetCount: -1,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects fractional subnet count", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      publicSubnetCount: 1.5,
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects invalid custom subnet CIDR in vpc-and-more mode", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      publicSubnetCidrs: ["not-a-cidr"],
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects subnet CIDR outside VPC range", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      publicSubnetCidrs: ["192.168.0.0/20"],
    })
    if (result.success) {
      throw new Error("expected validation to fail")
    }
    const issue = result.error.issues.find(
      (i) => i.path[0] === "publicSubnetCidrs",
    )
    expect(issue?.message).toContain("within the VPC CIDR")
  })

  it("rejects overlapping subnet CIDRs", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
      publicSubnetCidrs: ["10.0.0.0/20"],
      privateSubnetCidrs: ["10.0.0.0/20"],
    })
    if (result.success) {
      throw new Error("expected validation to fail")
    }
    const issue = result.error.issues.find((i) => i.message.includes("overlap"))
    expect(issue).toBeDefined()
  })

  it("accepts valid custom subnet CIDRs within VPC range", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      mode: "vpc-and-more",
      publicSubnetCount: 1,
      privateSubnetCount: 1,
      publicSubnetCidrs: ["10.0.0.0/20"],
      privateSubnetCidrs: ["10.0.128.0/20"],
    })
    expect(result.success).toBeTruthy()
  })

  it("skips subnet CIDR validation in vpc-only mode", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      mode: "vpc-only",
      publicSubnetCidrs: ["not-a-cidr"],
    })
    expect(result.success).toBeTruthy()
  })

  it("accepts valid tags", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      tags: [{ key: "Env", value: "prod" }],
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects tags with empty key", () => {
    const result = createVpcWizardSchema.safeParse({
      ...validBase,
      tags: [{ key: "", value: "prod" }],
    })
    expect(result.success).toBeFalsy()
  })
})
