import type { ResourceRecordSet } from "@aws-sdk/client-route-53"
import { z } from "zod"

import type { ActivationState } from "@/lib/route53-activation"

// A TXT character-string carries at most 255 bytes on the wire.
const MAX_TXT_STRING_BYTES = 255

const DOMAIN_NAME_REGEX =
  /^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.?$/

export const createHostedZoneSchema = z.object({
  name: z
    .string()
    .trim()
    .min(1, "Domain name is required")
    .max(253, "Domain name must be 253 characters or less")
    .regex(
      DOMAIN_NAME_REGEX,
      "Enter a domain name such as example.com, with at least two labels",
    ),
  comment: z
    .string()
    .max(256, "Comment must be 256 characters or less")
    .optional(),
})

export type CreateHostedZoneFormData = z.infer<typeof createHostedZoneSchema>

// The record types the console offers. The API accepts a few more (NS below
// the apex, PTR, SPF); those are shown but managed through the API.
export const EDITABLE_RECORD_TYPES = [
  "A",
  "AAAA",
  "CNAME",
  "MX",
  "TXT",
  "SRV",
  "CAA",
] as const

export type EditableRecordType = (typeof EDITABLE_RECORD_TYPES)[number]

export const RECORD_VALUE_HINTS: Record<EditableRecordType, string> = {
  A: "One IPv4 address per line, e.g. 192.0.2.10",
  AAAA: "One IPv6 address per line, e.g. 2001:db8::10",
  CNAME: "One domain name, e.g. www.example.com",
  MX: "Priority and mail server per line, e.g. 10 mail.example.com",
  TXT: "One value per line, as plain text. Quotes are added for you.",
  SRV: "Priority weight port target per line, e.g. 10 5 5060 sip.example.com",
  CAA: 'Flags tag "value" per line, e.g. 0 issue "letsencrypt.org"',
}

const IPV4_REGEX =
  /^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)$/

// Lenient: the gateway is the authority, this only catches obvious typos.
const IPV6_REGEX = /^[0-9a-fA-F:.]+$/
const HOST_REGEX = /^(?:[a-zA-Z0-9_*](?:[a-zA-Z0-9_-]*[a-zA-Z0-9])?\.?)+$/
const MX_REGEX = /^\d{1,5}\s+\S+$/
const SRV_REGEX = /^\d{1,5}\s+\d{1,5}\s+\d{1,5}\s+\S+$/
const CAA_REGEX = /^\d{1,3}\s+[a-zA-Z0-9]+\s+".*"$/

const VALUE_CHECKS: Record<
  EditableRecordType,
  { test: (value: string) => boolean; message: string }
> = {
  A: { test: (v) => IPV4_REGEX.test(v), message: "is not an IPv4 address" },
  AAAA: {
    test: (v) => IPV6_REGEX.test(v) && v.includes(":"),
    message: "is not an IPv6 address",
  },
  CNAME: { test: (v) => HOST_REGEX.test(v), message: "is not a domain name" },
  MX: {
    test: (v) => MX_REGEX.test(v),
    message: "must be a priority and a mail server",
  },
  TXT: { test: () => true, message: "" },
  SRV: {
    test: (v) => SRV_REGEX.test(v),
    message: "must be priority, weight, port and target",
  },
  CAA: {
    test: (v) => CAA_REGEX.test(v),
    message: 'must be flags, tag and a quoted value, e.g. 0 issue "ca.example"',
  },
}

export function recordValueLines(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "")
}

export const recordSetSchema = z
  .object({
    // Relative to the zone; empty means the apex.
    name: z
      .string()
      .trim()
      .regex(
        /^(?:\*|[a-zA-Z0-9_](?:[a-zA-Z0-9_-]*[a-zA-Z0-9_])?)?(?:\.[a-zA-Z0-9_](?:[a-zA-Z0-9_-]*[a-zA-Z0-9_])?)*$/,
        "Use letters, digits, hyphens and dots, or * for a wildcard",
      ),
    type: z.enum(EDITABLE_RECORD_TYPES),
    ttl: z
      .number({ error: "TTL must be a number" })
      .int("TTL must be a whole number")
      .min(0, "TTL must be at least 0")
      .max(2_147_483_647, "TTL is too large"),
    values: z.string(),
  })
  .superRefine((data, ctx) => {
    const lines = recordValueLines(data.values)

    if (lines.length === 0) {
      ctx.addIssue({
        code: "custom",
        path: ["values"],
        message: "At least one value is required",
      })

      return
    }

    if (data.type === "CNAME" && lines.length > 1) {
      ctx.addIssue({
        code: "custom",
        path: ["values"],
        message: "A CNAME record takes exactly one value",
      })
    }

    const check = VALUE_CHECKS[data.type]
    const bad = lines.find((line) => !check.test(line))

    if (bad !== undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["values"],
        message: `"${bad}" ${check.message}`,
      })
    }
  })

export type RecordSetFormData = z.infer<typeof recordSetSchema>

// Route 53 escapes characters outside the hostname set as \ddd octal, so a
// wildcard comes back as \052.
export function decodeRecordName(name: string): string {
  return name.replaceAll(/\\(?<octal>\d{3})/g, (_match, octal: string) =>
    String.fromCodePoint(Number.parseInt(octal, 8)),
  )
}

export function ensureTrailingDot(name: string): string {
  return name.endsWith(".") ? name : `${name}.`
}

export function sameName(a: string, b: string): boolean {
  return (
    ensureTrailingDot(decodeRecordName(a)).toLowerCase() ===
    ensureTrailingDot(decodeRecordName(b)).toLowerCase()
  )
}

// relativeRecordName is a record's name with the zone suffix removed, "" for
// the apex.
export function relativeRecordName(name: string, zoneName: string): string {
  const full = ensureTrailingDot(decodeRecordName(name))
  const zone = ensureTrailingDot(zoneName)

  if (full.toLowerCase() === zone.toLowerCase()) {
    return ""
  }

  const suffix = `.${zone}`

  return full.toLowerCase().endsWith(suffix.toLowerCase())
    ? full.slice(0, -suffix.length)
    : full
}

export function absoluteRecordName(label: string, zoneName: string): string {
  const zone = ensureTrailingDot(zoneName)

  return label === "" ? zone : `${label}.${zone}`
}

function utf8Length(text: string): number {
  return new TextEncoder().encode(text).length
}

function escapeTxt(text: string): string {
  return text.replaceAll("\\", "\\\\").replaceAll('"', '\\"')
}

// quoteTxt turns plain text into the quoted character-strings a TXT record
// carries, splitting it so no string exceeds 255 bytes once decoded.
export function quoteTxt(text: string): string {
  const chunks: string[] = []
  let current = ""
  let currentBytes = 0

  for (const char of text) {
    const bytes = utf8Length(char)

    if (currentBytes + bytes > MAX_TXT_STRING_BYTES) {
      chunks.push(current)
      current = ""
      currentBytes = 0
    }

    current += char
    currentBytes += bytes
  }

  chunks.push(current)

  return chunks.map((chunk) => `"${escapeTxt(chunk)}"`).join(" ")
}

// unquoteTxt reverses quoteTxt: it joins a value's character-strings and
// removes their escapes. A value with no opening quote is returned as is.
export function unquoteTxt(value: string): string {
  const trimmed = value.trim()

  if (!trimmed.startsWith('"')) {
    return trimmed
  }

  let out = ""
  let inString = false

  for (let i = 0; i < trimmed.length; i++) {
    const char = trimmed[i]

    if (char === '"') {
      inString = !inString
    } else if (inString && char === "\\") {
      const digits = trimmed.slice(i + 1, i + 4)

      if (/^\d{3}$/.test(digits)) {
        out += String.fromCodePoint(Number(digits))
        i += 3
      } else {
        i += 1
        out += trimmed[i] ?? ""
      }
    } else if (inString) {
      out += char
    }
  }

  return out
}

// toResourceRecords maps the form's lines onto the API's values, quoting TXT.
export function toResourceRecords(
  type: EditableRecordType,
  valuesText: string,
): { Value: string }[] {
  return recordValueLines(valuesText).map((line) => ({
    Value: type === "TXT" ? quoteTxt(line) : line,
  }))
}

export function toFormValues(
  recordSet: ResourceRecordSet,
  zoneName: string,
): RecordSetFormData {
  const type = isEditableType(recordSet.Type) ? recordSet.Type : "A"

  const values = (recordSet.ResourceRecords ?? [])
    .map((r) => r.Value ?? "")
    .map((v) => (type === "TXT" ? unquoteTxt(v) : v))

  return {
    name: relativeRecordName(recordSet.Name ?? "", zoneName),
    type,
    ttl: recordSet.TTL ?? 300,
    values: values.join("\n"),
  }
}

export function toRecordSet(
  data: RecordSetFormData,
  zoneName: string,
): ResourceRecordSet {
  return {
    Name: absoluteRecordName(data.name, zoneName),
    Type: data.type,
    TTL: data.ttl,
    ResourceRecords: toResourceRecords(data.type, data.values),
  }
}

export function isEditableType(
  type: string | undefined,
): type is EditableRecordType {
  return EDITABLE_RECORD_TYPES.some((t) => t === type)
}

// The apex NS and SOA belong to the zone itself, so the API refuses to delete
// them and the console does not offer to.
export function isZoneOwnedRecord(
  recordSet: ResourceRecordSet,
  zoneName: string,
): boolean {
  return (
    (recordSet.Type === "NS" || recordSet.Type === "SOA") &&
    sameName(recordSet.Name ?? "", zoneName)
  )
}

export const ACTIVATION_LABELS: Record<ActivationState, string> = {
  PENDING_NAMESERVERS: "Pending nameservers",
  ACTIVE: "Active",
  NAMESERVERS_LOST: "Nameservers lost",
  SUSPENDED: "Suspended",
}
