import { z } from "zod"

import { jsonStringSchema } from "@/lib/json"

export const createUserSchema = z.object({
  userName: z
    .string()
    .min(1, "User name is required")
    .max(64, "User name must be 64 characters or less")
    .regex(/^[\w+=,.@-]+$/, "User name contains invalid characters"),
  path: z.string().optional(),
})

export type CreateUserFormData = z.infer<typeof createUserSchema>

export const createPolicySchema = z.object({
  policyName: z
    .string()
    .min(1, "Policy name is required")
    .max(128, "Policy name must be 128 characters or less")
    .regex(/^[\w+=,.@-]+$/, "Policy name contains invalid characters"),
  description: z.string().optional(),
  policyDocument: jsonStringSchema({ label: "Policy document" }),
})

export type CreatePolicyFormData = z.infer<typeof createPolicySchema>

export const DEFAULT_ASSUME_ROLE_POLICY_DOCUMENT = JSON.stringify(
  {
    Version: "2012-10-17",
    Statement: [
      {
        Effect: "Allow",
        Principal: { Service: "ec2.amazonaws.com" },
        Action: "sts:AssumeRole",
      },
    ],
  },
  null,
  2,
)

export const createRoleSchema = z.object({
  roleName: z
    .string()
    .min(1, "Role name is required")
    .max(64, "Role name must be 64 characters or less")
    .regex(/^[\w+=,.@-]+$/, "Role name contains invalid characters"),
  path: z.string().optional(),
  description: z.string().optional(),
  assumeRolePolicyDocument: jsonStringSchema({ label: "Trust policy" }),
})

export type CreateRoleFormData = z.infer<typeof createRoleSchema>

export const createInstanceProfileSchema = z.object({
  instanceProfileName: z
    .string()
    .min(1, "Instance profile name is required")
    .max(128, "Instance profile name must be 128 characters or less")
    .regex(
      /^[\w+=,.@-]+$/,
      "Instance profile name contains invalid characters",
    ),
  path: z.string().optional(),
})

export type CreateInstanceProfileFormData = z.infer<
  typeof createInstanceProfileSchema
>

export const createGroupSchema = z.object({
  groupName: z
    .string()
    .min(1, "Group name is required")
    .max(128, "Group name must be 128 characters or less")
    .regex(/^[\w+=,.@-]+$/, "Group name contains invalid characters"),
  path: z.string().optional(),
})

export type CreateGroupFormData = z.infer<typeof createGroupSchema>

export interface DeleteAccessKeyParams {
  userName: string
  accessKeyId: string
}

export interface UpdateAccessKeyParams {
  userName: string
  accessKeyId: string
  status: "Active" | "Inactive"
}

export interface UserPolicyParams {
  userName: string
  policyArn: string
}

export interface RolePolicyParams {
  roleName: string
  policyArn: string
}

export interface AddRoleToProfileParams {
  instanceProfileName: string
  roleName: string
}

export interface AssociateInstanceProfileParams {
  instanceId: string
  instanceProfileName: string
}

export interface GroupPolicyParams {
  groupName: string
  policyArn: string
}

export interface GroupMembershipParams {
  groupName: string
  userName: string
}
