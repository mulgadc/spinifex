import { z } from "zod"

export function isValidJson(value: string): boolean {
  try {
    JSON.parse(value)
    return true
  } catch {
    return false
  }
}

// Zod field for a JSON document entered as a string. Messages derive from
// `label` so they read naturally per field. When `allowEmpty` is true an
// empty/whitespace value passes (optional field); otherwise it is required.
export function jsonStringSchema(opts: {
  label: string
  allowEmpty?: boolean
}) {
  const { label, allowEmpty = false } = opts
  const base = allowEmpty
    ? z.string()
    : z.string().min(1, `${label} is required`)
  return base.refine(
    (value) => (allowEmpty && value.trim() === "" ? true : isValidJson(value)),
    { message: `${label} must be valid JSON` },
  )
}
