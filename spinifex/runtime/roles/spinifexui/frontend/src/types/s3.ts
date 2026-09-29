import { z } from "zod"

export const createBucketSchema = z.object({
  bucketName: z
    .string()
    .min(3, "Bucket name must be at least 3 characters")
    .max(63, "Bucket name must be at most 63 characters")
    .regex(
      /^[a-z0-9][a-z0-9.-]*[a-z0-9]$/,
      "Must start and end with a lowercase letter or number",
    )
    .regex(
      /^[a-z0-9.-]+$/,
      "Only lowercase letters, numbers, hyphens, and periods allowed",
    )
    .refine(
      (name) => !name.includes(".."),
      "Must not contain consecutive periods",
    )
    .refine(
      (name) => !(name.includes(".-") || name.includes("-.")),
      "Must not contain adjacent period and hyphen",
    ),
})

export type CreateBucketFormData = z.infer<typeof createBucketSchema>
