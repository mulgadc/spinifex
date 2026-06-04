import { z } from "zod"

import {
  cidrContains,
  cidrsOverlap,
  isValidCidr,
} from "@/lib/subnet-calculator"

const CIDR_REGEX = /^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\/\d{1,2}$/

const keyNameField = z
  .string()
  .min(1, "Key name is required")
  .max(255, "Key name must be 255 characters or less")
  .regex(
    /^[\w\s._\-:/()#,@[\]+=&;{}!$*]+$/,
    "Key name contains invalid characters",
  )

export const VOLUME_TYPES = ["gp3"] as const

// Quick-create inbound rules offered by the launch wizard, mirroring the AWS
// console's "Allow SSH/HTTP/HTTPS" tick boxes.
export const LAUNCH_WIZARD_SG_PREFIX = "launch-wizard-"

export const createInstanceSchema = z
  .object({
    imageId: z.string("Please select an Image"),
    instanceType: z.string("Please select an instance type"),
    keyName: z
      .string("Please select a key pair")
      .min(1, "Key pair is required"),
    subnetId: z.string().optional(),
    placementGroupName: z.string().optional(),
    count: z
      .int("Instance count must be a whole number")
      .min(1, "Instance count must be at least 1"),
    rootDeviceName: z.string().optional(),
    rootVolumeSize: z
      .number()
      .int("Volume size must be a whole number")
      .min(1, "Volume size must be at least 1 GiB")
      .max(16_384, "Volume size must be at most 16384 GiB")
      .optional(),
    rootVolumeType: z.enum(VOLUME_TYPES).optional(),
    rootDeleteOnTermination: z.boolean().optional(),
    // Security groups: either create a new launch-wizard SG with tick-box
    // rules, or attach existing SG(s). Mirrors the AWS launch wizard. All
    // optional so non-UI callers keep today's no-SG behaviour; the form always
    // sets a mode, and the mutation only creates an SG when mode === "create".
    securityGroupMode: z.enum(["create", "existing"]).optional(),
    securityGroupIds: z.array(z.string()).optional(),
    newSgName: z.string().optional(),
    newSgDescription: z.string().optional(),
    allowSsh: z.boolean().optional(),
    allowHttp: z.boolean().optional(),
    allowHttps: z.boolean().optional(),
    ruleSource: z.enum(["anywhere", "custom"]).optional(),
    customCidr: z.string().optional(),
  })
  .superRefine((data, ctx) => {
    if (!data.securityGroupMode) {
      return
    }
    if (data.securityGroupMode === "existing") {
      if ((data.securityGroupIds ?? []).length === 0) {
        ctx.addIssue({
          code: "custom",
          message: "Select at least one security group",
          path: ["securityGroupIds"],
        })
      }
      return
    }
    if (!data.newSgName || data.newSgName.trim().length === 0) {
      ctx.addIssue({
        code: "custom",
        message: "Security group name is required",
        path: ["newSgName"],
      })
    }
    if (
      data.ruleSource === "custom" &&
      (!data.customCidr || !isValidCidr(data.customCidr))
    ) {
      ctx.addIssue({
        code: "custom",
        message: "Enter a valid CIDR (e.g. 203.0.113.0/24)",
        path: ["customCidr"],
      })
    }
  })

export type CreateInstanceFormData = z.infer<typeof createInstanceSchema>

export type CreateInstanceParams = CreateInstanceFormData & {
  // VPC the chosen subnet belongs to (or the default VPC when none is
  // selected). Resolved in the form so the mutation can create the SG in the
  // right VPC.
  resolvedVpcId?: string
}

// nextLaunchWizardName returns the next "launch-wizard-N" name not already
// taken by an existing SG, matching the AWS console's auto-increment.
export function nextLaunchWizardName(existingNames: string[]): string {
  let max = 0
  const re = new RegExp(`^${LAUNCH_WIZARD_SG_PREFIX}(\\d+)$`)
  for (const name of existingNames) {
    const m = re.exec(name)
    if (m) {
      const n = Number(m[1])
      if (n > max) {
        max = n
      }
    }
  }
  return `${LAUNCH_WIZARD_SG_PREFIX}${max + 1}`
}

export const createKeyPairSchema = z.object({
  keyName: keyNameField,
})

export type CreateKeyPairData = z.infer<typeof createKeyPairSchema>

export const importKeyPairSchema = z.object({
  keyName: keyNameField,
  publicKeyMaterial: z
    .string()
    .min(1, "Public key is required")
    .refine((key) => key.trim().length > 0, "Public key cannot be empty"),
})

export type ImportKeyPairData = z.infer<typeof importKeyPairSchema>

export const createVolumeSchema = z.object({
  size: z
    .number()
    .int("Size must be a whole number")
    .min(1, "Size must be at least 1 GiB")
    .max(16_384, "Size must be at most 16384 GiB"),
  availabilityZone: z.string().min(1, "Availability zone is required"),
})

export type CreateVolumeFormData = z.infer<typeof createVolumeSchema>

export const modifyVolumeSchema = z.object({
  size: z
    .number()
    .int("Size must be a whole number")
    .min(1, "Size must be at least 1 GiB"),
})

export type ModifyVolumeFormData = z.infer<typeof modifyVolumeSchema>

export type ModifyVolumeParams = ModifyVolumeFormData & { volumeId: string }

export const createSnapshotSchema = z.object({
  volumeId: z.string().min(1, "Volume is required"),
  description: z.string().optional(),
})

export type CreateSnapshotFormData = z.infer<typeof createSnapshotSchema>

export const copySnapshotSchema = z.object({
  sourceSnapshotId: z.string().min(1, "Source snapshot is required"),
  sourceRegion: z.string().min(1, "Source region is required"),
  description: z.string().optional(),
})

export type CopySnapshotFormData = z.infer<typeof copySnapshotSchema>

export const attachVolumeSchema = z.object({
  volumeId: z.string().min(1, "Volume is required"),
  instanceId: z.string().min(1, "Instance is required"),
  device: z.string().optional(),
})

export type AttachVolumeFormData = z.infer<typeof attachVolumeSchema>

export const detachVolumeSchema = z.object({
  volumeId: z.string().min(1, "Volume is required"),
  instanceId: z.string().optional(),
  force: z.boolean().optional(),
})

export type DetachVolumeFormData = z.infer<typeof detachVolumeSchema>

export const modifyInstanceTypeSchema = z.object({
  instanceType: z.string().min(1, "Instance type is required"),
})

export type ModifyInstanceTypeFormData = z.infer<
  typeof modifyInstanceTypeSchema
>

export const createImageSchema = z.object({
  name: z.string().min(1, "Name is required"),
  description: z.string().optional(),
})

export type CreateImageFormData = z.infer<typeof createImageSchema>

export type CreateImageParams = CreateImageFormData & { instanceId: string }

export const createSubnetSchema = z.object({
  vpcId: z.string().min(1, "VPC is required"),
  cidrBlock: z
    .string()
    .min(1, "CIDR block is required")
    .regex(CIDR_REGEX, "Must be a valid CIDR block (e.g. 10.0.1.0/24)"),
  availabilityZone: z.string().optional(),
  // AWS subnet console "Auto-assign public IPv4 address". CreateSubnet
  // defaults this off; a follow-up ModifySubnetAttribute turns it on.
  mapPublicIpOnLaunch: z.boolean().optional(),
})

export type CreateSubnetFormData = z.infer<typeof createSubnetSchema>

export const allocateAddressSchema = z.object({
  name: z.string().optional(),
})

export type AllocateAddressFormData = z.infer<typeof allocateAddressSchema>

export const createNatGatewaySchema = z.object({
  subnetId: z.string().min(1, "Subnet is required"),
  allocationId: z.string().min(1, "Elastic IP is required"),
  name: z.string().optional(),
})

export type CreateNatGatewayFormData = z.infer<typeof createNatGatewaySchema>

export const createInternetGatewaySchema = z.object({
  name: z.string().optional(),
})

export type CreateInternetGatewayFormData = z.infer<
  typeof createInternetGatewaySchema
>

export const createVpcSchema = z.object({
  cidrBlock: z
    .string()
    .min(1, "CIDR block is required")
    .regex(CIDR_REGEX, "Must be a valid CIDR block (e.g. 10.0.0.0/16)"),
  name: z.string().optional(),
})

export type CreateVpcFormData = z.infer<typeof createVpcSchema>

export const formTagSchema = z.object({
  key: z.string().min(1, "Key is required"),
  value: z.string(),
})

export type FormTag = z.infer<typeof formTagSchema>

export const createVpcWizardSchema = z
  .object({
    mode: z.enum(["vpc-only", "vpc-and-more"]),
    namePrefix: z.string().optional(),
    autoGenerateNames: z.boolean(),
    cidrBlock: z
      .string()
      .min(1, "CIDR block is required")
      .regex(CIDR_REGEX, "Must be a valid CIDR block (e.g. 10.0.0.0/16)")
      .refine(
        (cidr) => isValidCidr(cidr),
        "CIDR has invalid octets or prefix length (must be /16 to /28)",
      ),
    tenancy: z.enum(["default", "dedicated"]),
    // NAT gateway for private-subnet egress. Optional so existing callers/tests
    // default to "none"; "single" provisions one NAT GW (+ Elastic IP) in the
    // first public subnet and routes private 0.0.0.0/0 to it.
    natGateway: z.enum(["none", "single"]).optional(),
    publicSubnetCount: z.number().int().min(0).max(1),
    privateSubnetCount: z.number().int().min(0).max(2),
    publicSubnetCidrs: z.array(z.string()),
    privateSubnetCidrs: z.array(z.string()),
    tags: z.array(formTagSchema),
  })
  .superRefine((data, ctx) => {
    if (data.mode !== "vpc-and-more") {
      return
    }

    const allCidrs: { cidr: string; field: string; index: number }[] = []

    for (const [i, cidr] of data.publicSubnetCidrs.entries()) {
      if (isValidCidr(cidr, 16, 28)) {
        if (!cidrContains(data.cidrBlock, cidr)) {
          ctx.addIssue({
            code: "custom",
            message: "Subnet CIDR must be within the VPC CIDR range",
            path: ["publicSubnetCidrs", i],
          })
        }
        allCidrs.push({ cidr, field: "publicSubnetCidrs", index: i })
      } else {
        ctx.addIssue({
          code: "custom",
          message: "Invalid subnet CIDR format or prefix length",
          path: ["publicSubnetCidrs", i],
        })
      }
    }

    for (const [i, cidr] of data.privateSubnetCidrs.entries()) {
      if (isValidCidr(cidr, 16, 28)) {
        if (!cidrContains(data.cidrBlock, cidr)) {
          ctx.addIssue({
            code: "custom",
            message: "Subnet CIDR must be within the VPC CIDR range",
            path: ["privateSubnetCidrs", i],
          })
        }
        allCidrs.push({ cidr, field: "privateSubnetCidrs", index: i })
      } else {
        ctx.addIssue({
          code: "custom",
          message: "Invalid subnet CIDR format or prefix length",
          path: ["privateSubnetCidrs", i],
        })
      }
    }

    for (let i = 0; i < allCidrs.length; i++) {
      for (let j = i + 1; j < allCidrs.length; j++) {
        const a = allCidrs[i]
        const b = allCidrs[j]
        if (a && b && cidrsOverlap(a.cidr, b.cidr)) {
          ctx.addIssue({
            code: "custom",
            message: "Subnet CIDRs must not overlap",
            path: [b.field, b.index],
          })
        }
      }
    }
  })

export type CreateVpcWizardFormData = z.infer<typeof createVpcWizardSchema>

export const createPlacementGroupSchema = z.object({
  groupName: z
    .string()
    .min(1, "Group name is required")
    .max(255, "Group name must be 255 characters or less")
    .regex(
      /^[\w\s._\-:/()#,@[\]+=&;{}!$*]+$/,
      "Group name contains invalid characters",
    ),
  strategy: z.string().min(1, "Strategy is required"),
})

export type CreatePlacementGroupFormData = z.infer<
  typeof createPlacementGroupSchema
>

export const createSecurityGroupSchema = z.object({
  groupName: z
    .string()
    .min(1, "Group name is required")
    .max(255, "Group name must be 255 characters or less"),
  description: z
    .string()
    .min(1, "Description is required")
    .max(255, "Description must be 255 characters or less"),
  vpcId: z.string().min(1, "VPC is required"),
})

export type CreateSecurityGroupFormData = z.infer<
  typeof createSecurityGroupSchema
>

export const securityGroupRuleSchema = z.object({
  ipProtocol: z.string().min(1, "Protocol is required"),
  fromPort: z.number().int().min(-1).max(65_535),
  toPort: z.number().int().min(-1).max(65_535),
  cidrIp: z
    .string()
    .min(1, "CIDR is required")
    .regex(CIDR_REGEX, "Must be a valid CIDR block (e.g. 0.0.0.0/0)"),
})

export type SecurityGroupRuleFormData = z.infer<typeof securityGroupRuleSchema>
