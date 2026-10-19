import { z } from "zod"

const TAG_KEY_MAX_LENGTH = 128
const TAG_VALUE_MAX_LENGTH = 256

// AWS's tag limits, common to every service the console tags through: a key of
// 1-128 characters and a value of 0-256.
export const tagSchema = z.object({
  key: z
    .string()
    .min(1, "Tag key is required")
    .max(
      TAG_KEY_MAX_LENGTH,
      `Tag key must be ${TAG_KEY_MAX_LENGTH} characters or less`,
    ),
  value: z
    .string()
    .max(
      TAG_VALUE_MAX_LENGTH,
      `Tag value must be ${TAG_VALUE_MAX_LENGTH} characters or less`,
    ),
})

export type TagFormData = z.infer<typeof tagSchema>

// Structural rather than one SDK's Tag: every service declares the same shape
// in its own package, and this is assignable to each of them.
interface SdkTag {
  Key: string
  Value: string
}

// Rows with a blank key are dropped, and an empty set becomes undefined so the
// request omits Tags rather than sending an empty list.
export function toSdkTags(tags: TagFormData[]): SdkTag[] | undefined {
  const set = tags.flatMap((t) =>
    t.key.length > 0 ? [{ Key: t.key, Value: t.value }] : [],
  )

  return set.length > 0 ? set : undefined
}

export interface TagDiff {
  // Sent whole: an add-tags call overwrites existing keys, so it covers both
  // added and changed tags.
  toSet: SdkTag[] | undefined
  // Keys present before the edit that the desired set no longer carries.
  toRemove: string[]
}

export function diffTags(
  desired: TagFormData[],
  initialKeys: string[],
): TagDiff {
  const toSet = toSdkTags(desired)
  const finalKeys = new Set(toSet?.map((t) => t.Key))

  return {
    toSet,
    toRemove: initialKeys.filter((k) => !finalKeys.has(k)),
  }
}
