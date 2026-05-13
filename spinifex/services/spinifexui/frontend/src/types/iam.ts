import { z } from "zod"

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
  policyDocument: z
    .string()
    .min(1, "Policy document is required")
    .refine(
      (val) => {
        try {
          JSON.parse(val)
          return true
        } catch {
          return false
        }
      },
      { message: "Policy document must be valid JSON" },
    ),
})

export type CreatePolicyFormData = z.infer<typeof createPolicySchema>

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
