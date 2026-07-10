import { z } from "zod"

export function isValidJson(value: string): boolean {
  try {
    JSON.parse(value)
    return true
  } catch {
    return false
  }
}

// Pretty-print JSON with a 2-space indent. Returns null when the input is not
// valid JSON so callers can leave the original text untouched.
export function formatJson(value: string): string | null {
  try {
    return JSON.stringify(JSON.parse(value), null, 2)
  } catch {
    return null
  }
}

// Normalise an AWS policy document for display in an editor. Most principals
// return the document URL-encoded, but roles return raw JSON, so callers pass
// `encoded` to control decoding — decoding raw JSON could corrupt literal
// percent sequences. The result is pretty-printed when it parses.
export function decodePolicyDocument(
  document: string,
  encoded: boolean,
): string {
  let text = document
  if (encoded) {
    try {
      text = decodeURIComponent(document)
    } catch {
      text = document
    }
  }
  return formatJson(text) ?? text
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
