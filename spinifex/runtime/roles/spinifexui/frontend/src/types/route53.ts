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

// The record types the console offers. The API accepts a few more (PTR, SPF);
// those are shown but managed through the API.
export const EDITABLE_RECORD_TYPES = [
  "A",
  "AAAA",
  "CNAME",
  "MX",
  "TXT",
  "NS",
  "SRV",
  "CAA",
] as const

export type EditableRecordType = (typeof EDITABLE_RECORD_TYPES)[number]

// What the value field is called, since "value" means something different for
// every type.
export const RECORD_VALUE_LABELS: Record<EditableRecordType, string> = {
  A: "IPv4 address",
  AAAA: "IPv6 address",
  CNAME: "Target",
  MX: "Mail server",
  TXT: "Content",
  NS: "Name server",
  SRV: "Service target",
  CAA: "Certificate authority",
}

export const RECORD_VALUE_HINTS: Record<EditableRecordType, string> = {
  A: "One IPv4 address per line, e.g. 192.0.2.10",
  AAAA: "One IPv6 address per line, e.g. 2001:db8::10",
  CNAME: "One domain name, e.g. www.example.com",
  MX: "One mail server per line, e.g. mail.example.com. A line may carry its own priority, as 20 backup.example.com.",
  TXT: "One value per line, as plain text. Quotes are added for you.",
  NS: "One name server per line, e.g. ns1.example.net",
  SRV: "Priority weight port target per line, e.g. 10 5 5060 sip.example.com",
  CAA: 'Flags tag "value" per line, e.g. 0 issue "letsencrypt.org"',
}

export const RECORD_TYPE_HELP: readonly (readonly [
  EditableRecordType,
  string,
])[] = [
  ["A", "Points a name at an IPv4 address."],
  ["AAAA", "Points a name at an IPv6 address."],
  ["CNAME", "Makes a name an alias of another name."],
  ["MX", "Names the mail servers for the domain, lowest priority first."],
  ["TXT", "Free text, such as SPF or a domain verification code."],
  ["NS", "Delegates a subdomain to other name servers."],
  ["SRV", "Locates a service: priority, weight, port and host."],
  ["CAA", "Says which certificate authorities may issue for the name."],
]

// "Auto" is 300 seconds, so there is no separate 5 minutes entry.
export const DEFAULT_TTL_SECONDS = 300

export const TTL_OPTIONS: readonly { value: number; label: string }[] = [
  { value: DEFAULT_TTL_SECONDS, label: "Auto (5 min)" },
  { value: 60, label: "1 min" },
  { value: 120, label: "2 min" },
  { value: 600, label: "10 min" },
  { value: 900, label: "15 min" },
  { value: 1800, label: "30 min" },
  { value: 3600, label: "1 hr" },
  { value: 7200, label: "2 hr" },
  { value: 18_000, label: "5 hr" },
  { value: 43_200, label: "12 hr" },
  { value: 86_400, label: "1 day" },
]

export const DEFAULT_MX_PRIORITY = 10

const IPV4_REGEX =
  /^(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)$/

// Lenient: the gateway is the authority, this only catches obvious typos.
const IPV6_REGEX = /^[0-9a-fA-F:.]+$/
const HOST_REGEX = /^(?:[a-zA-Z0-9_*](?:[a-zA-Z0-9_-]*[a-zA-Z0-9])?\.?)+$/
const MX_REGEX = /^\d{1,5}\s+\S+$/
const MX_PRIORITY_REGEX = /^(?<priority>\d{1,5})\s+(?<server>\S+)$/
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
    test: (v) => MX_REGEX.test(v) || HOST_REGEX.test(v),
    message: "is not a mail server",
  },
  TXT: { test: () => true, message: "" },
  NS: { test: (v) => HOST_REGEX.test(v), message: "is not a name server" },
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
    // MX only: applied to every server line that does not carry its own.
    priority: z
      .number({ error: "Priority must be a number" })
      .int("Priority must be a whole number")
      .min(0, "Priority must be at least 0")
      .max(65_535, "Priority must be 65535 or less"),
    values: z.string(),
  })
  .superRefine((data, ctx) => {
    if (data.type === "NS" && data.name === "") {
      ctx.addIssue({
        code: "custom",
        path: ["name"],
        message:
          "The zone manages its own name servers. Enter a subdomain to delegate it.",
      })
    }

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

function toRecordValue(
  type: EditableRecordType,
  line: string,
  priority: number,
): string {
  if (type === "TXT") {
    return quoteTxt(line)
  }

  if (type === "MX" && !MX_REGEX.test(line)) {
    return `${priority} ${line}`
  }

  return line
}

// toResourceRecords maps the form's lines onto the API's values, quoting TXT
// and putting the priority before an MX server that has none of its own.
export function toResourceRecords(
  type: EditableRecordType,
  valuesText: string,
  priority: number = DEFAULT_MX_PRIORITY,
): { Value: string }[] {
  return recordValueLines(valuesText).map((line) => ({
    Value: toRecordValue(type, line, priority),
  }))
}

// splitMxValues lifts a priority every server shares into its own field, and
// leaves mixed priorities on their lines so none is rewritten.
interface MxFormValues {
  priority: number
  lines: string[]
}

export function splitMxValues(values: readonly string[]): MxFormValues {
  const parsed = values.map((value) => MX_PRIORITY_REGEX.exec(value.trim()))
  const priorities = new Set(parsed.map((m) => m?.groups?.priority))
  const [shared] = priorities

  if (priorities.size === 1 && shared !== undefined) {
    return {
      priority: Number(shared),
      lines: parsed.map((m) => m?.groups?.server ?? ""),
    }
  }

  return { priority: DEFAULT_MX_PRIORITY, lines: [...values] }
}

export function toFormValues(
  recordSet: ResourceRecordSet,
  zoneName: string,
): RecordSetFormData {
  const type = isEditableType(recordSet.Type) ? recordSet.Type : "A"

  const values = (recordSet.ResourceRecords ?? [])
    .map((r) => r.Value ?? "")
    .map((v) => (type === "TXT" ? unquoteTxt(v) : v))

  const mx: MxFormValues =
    type === "MX"
      ? splitMxValues(values)
      : { priority: DEFAULT_MX_PRIORITY, lines: values }

  return {
    name: relativeRecordName(recordSet.Name ?? "", zoneName),
    type,
    ttl: recordSet.TTL ?? DEFAULT_TTL_SECONDS,
    priority: mx.priority,
    values: mx.lines.join("\n"),
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
    ResourceRecords: toResourceRecords(data.type, data.values, data.priority),
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
  PENDING_NAMESERVERS: "pending nameservers",
  ACTIVE: "active",
  NAMESERVERS_LOST: "nameservers lost",
  SUSPENDED: "suspended",
}
