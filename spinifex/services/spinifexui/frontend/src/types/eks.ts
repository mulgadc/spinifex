import type { AMITypes, CapacityTypes } from "@aws-sdk/client-eks"
import { z } from "zod"

import { jsonStringSchema } from "@/lib/json"
import { isValidCidr } from "@/lib/subnet-calculator"

// K3s control plane is pinned per Mulga release; expose the supported
// Kubernetes minor versions the backend accepts.
export const EKS_SUPPORTED_VERSIONS = ["1.32", "1.31", "1.30"] as const

// Only these AWS-managed access policies are mapped to K8s ClusterRoles in v1.
export const EKS_ACCESS_POLICIES = [
  {
    arn: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy",
    name: "AmazonEKSClusterAdminPolicy",
  },
  {
    arn: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSAdminPolicy",
    name: "AmazonEKSAdminPolicy",
  },
  {
    arn: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy",
    name: "AmazonEKSEditPolicy",
  },
  {
    arn: "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy",
    name: "AmazonEKSViewPolicy",
  },
] as const

export const EKS_AMI_TYPES = ["AL2_x86_64", "AL2_ARM_64"] as const
export const EKS_CAPACITY_TYPES = ["ON_DEMAND", "SPOT"] as const

// Trust policy for an EKS cluster IAM role: the EKS control plane assumes it.
export const EKS_CLUSTER_ASSUME_ROLE_POLICY_DOCUMENT = JSON.stringify(
  {
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Principal: { Service: "eks.amazonaws.com" },
        Action: "sts:AssumeRole",
      },
    ],
  },
  null,
  2,
)

// Managed policy granting the EKS control plane the permissions it needs.
export const AMAZON_EKS_CLUSTER_POLICY_ARN =
  "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"

const eksNameField = z
  .string()
  .min(1, "Name is required")
  .max(100, "Name must be 100 characters or fewer")
  .regex(
    /^[0-9A-Za-z][A-Za-z0-9\-_]*$/,
    "Must start with a letter or digit and contain only letters, digits, hyphens, or underscores",
  )

export const createClusterSchema = z
  .object({
    name: eksNameField,
    version: z.string().min(1, "Version is required"),
    roleArn: z.string().min(1, "Cluster IAM role is required"),
    vpcId: z.string().min(1, "VPC is required"),
    subnetIds: z.array(z.string()).min(1, "At least 1 subnet is required"),
    bootstrapClusterCreatorAdminPermissions: z.boolean(),
    endpointPublicAccess: z.boolean(),
    endpointPrivateAccess: z.boolean(),
    publicAccessCidrs: z.array(z.string()),
  })
  // The control plane is unreachable with both endpoints disabled; the backend
  // rejects this combination too.
  .refine((v) => v.endpointPublicAccess || v.endpointPrivateAccess, {
    message: "Enable public access, private access, or both",
    path: ["endpointPublicAccess"],
  })
  .refine(
    (v) =>
      !v.endpointPublicAccess ||
      v.publicAccessCidrs.every((c) => isValidCidr(c, 0, 32)),
    {
      message: "Each public access CIDR must be valid (e.g. 203.0.113.0/24)",
      path: ["publicAccessCidrs"],
    },
  )

export type CreateClusterFormData = z.infer<typeof createClusterSchema>

export const createNodegroupSchema = z
  .object({
    nodegroupName: eksNameField,
    nodeRole: z.string().min(1, "Node IAM role is required"),
    subnetIds: z.array(z.string()).min(1, "At least 1 subnet is required"),
    instanceTypes: z.string().min(1, "At least 1 instance type is required"),
    amiType: z.enum(EKS_AMI_TYPES),
    capacityType: z.enum(EKS_CAPACITY_TYPES),
    diskSize: z.number().int().min(1),
    minSize: z.number().int().min(0),
    desiredSize: z.number().int().min(0),
    maxSize: z.number().int().min(1),
  })
  .refine((v) => v.minSize <= v.desiredSize && v.desiredSize <= v.maxSize, {
    message: "Sizes must satisfy min ≤ desired ≤ max",
    path: ["desiredSize"],
  })

export type CreateNodegroupFormValues = z.infer<typeof createNodegroupSchema>

export const addonConfigurationSchema = jsonStringSchema({
  label: "Configuration",
  allowEmpty: true,
})

export const createAddonSchema = z.object({
  addonName: z.string().min(1, "Add-on is required"),
  addonVersion: z.string().min(1, "Version is required"),
  serviceAccountRoleArn: z.string(),
  configurationValues: addonConfigurationSchema,
})

export type CreateAddonFormValues = z.infer<typeof createAddonSchema>

// Map an add-on status to a Badge variant. In-progress states render calmly
// (secondary) rather than as errors; only terminal failures are destructive.
export function addonStatusVariant(
  status: string | undefined,
): "default" | "secondary" | "destructive" {
  switch (status ?? "") {
    case "ACTIVE": {
      return "default"
    }
    case "CREATE_FAILED":
    case "DELETE_FAILED":
    case "UPDATE_FAILED":
    case "DEGRADED": {
      return "destructive"
    }
    default: {
      return "secondary"
    }
  }
}

export const createAccessEntrySchema = z.object({
  principalArn: z.string().min(1, "Principal ARN is required"),
  kubernetesGroups: z.string(),
})

export type CreateAccessEntryFormValues = z.infer<
  typeof createAccessEntrySchema
>

export interface CreateNodegroupFormData {
  clusterName: string
  nodegroupName: string
  nodeRole: string
  subnetIds: string[]
  instanceTypes: string[]
  amiType?: AMITypes
  capacityType?: CapacityTypes
  diskSize?: number
  minSize: number
  maxSize: number
  desiredSize: number
}

export interface ScaleNodegroupParams {
  clusterName: string
  nodegroupName: string
  minSize: number
  maxSize: number
  desiredSize: number
}

export interface CreateAccessEntryFormData {
  clusterName: string
  principalArn: string
  kubernetesGroups: string[]
  type?: string
}

export interface AccessEntryParams {
  clusterName: string
  principalArn: string
}

export interface CreateAddonParams {
  clusterName: string
  addonName: string
  addonVersion?: string
  serviceAccountRoleArn?: string
  configurationValues?: string
}

export interface UpdateAddonParams {
  clusterName: string
  addonName: string
  addonVersion?: string
  serviceAccountRoleArn?: string
  configurationValues?: string
}

export interface DeleteAddonParams {
  clusterName: string
  addonName: string
}

export interface AssociateAccessPolicyParams {
  clusterName: string
  principalArn: string
  policyArn: string
  accessScopeType: "cluster" | "namespace"
  namespaces?: string[]
}

export interface DisassociateAccessPolicyParams {
  clusterName: string
  principalArn: string
  policyArn: string
}
