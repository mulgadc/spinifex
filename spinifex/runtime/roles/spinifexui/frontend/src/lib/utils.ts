import { createCn } from "cn/engine"

import tables from "./cn-tables.gen"

export const cn = createCn(tables)

const DATE_PARTS = {
  year: "numeric",
  month: "2-digit",
  day: "2-digit",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
  timeZoneName: "shortOffset",
} satisfies Intl.DateTimeFormatOptions

const localDateFormatter = new Intl.DateTimeFormat("en-US", DATE_PARTS)

// The AWS console's form, in the viewer's zone: "2026/06/04 02:00 GMT-5".
export function formatDateTime(
  date: Date | string | undefined,
  timeZone?: string,
): string {
  if (!date) {
    return "Unknown"
  }

  const formatter = timeZone
    ? new Intl.DateTimeFormat("en-US", { ...DATE_PARTS, timeZone })
    : localDateFormatter

  const parts = Object.fromEntries(
    formatter
      .formatToParts(new Date(date))
      .map((part) => [part.type, part.value]),
  )

  // ICU builds disagree on UTC, some writing "GMT+0"; the console writes "GMT".
  const zone = parts.timeZoneName === "GMT+0" ? "GMT" : parts.timeZoneName

  return `${parts.year}/${parts.month}/${parts.day} ${parts.hour}:${parts.minute} ${zone}`
}

export function formatVRAMMiB(mib: number): string {
  return `${Math.round(mib / 1024)} GiB`
}

const SIZE_UNITS = ["B", "KB", "MB", "GB", "TB"]

export function formatSize(bytes: number): string {
  if (bytes === 0) {
    return "0 B"
  }

  const i = Math.floor(Math.log(bytes) / Math.log(1024))

  return `${(bytes / 1024 ** i).toFixed(2)} ${SIZE_UNITS[i]}`
}

export function getNameTag(
  tags?: { Key?: string; Value?: string }[],
): string | undefined {
  return tags?.find((t) => t.Key === "Name")?.Value
}

// A security group's name is optional on the wire, so it is appended only when
// there is one rather than rendered as "sg-0abc123 (undefined)".
export function securityGroupLabel(group: {
  GroupId?: string
  GroupName?: string
}): string {
  const id = group.GroupId ?? ""

  return group.GroupName ? `${id} (${group.GroupName})` : id
}

// An unnamed VPC is told apart by its CIDR rather than by its bare id.
export function vpcLabel(vpc: {
  VpcId?: string
  CidrBlock?: string
  Tags?: { Key?: string; Value?: string }[]
}): string {
  const name = getNameTag(vpc.Tags)

  return name ? `${vpc.VpcId} (${name})` : `${vpc.VpcId} (${vpc.CidrBlock})`
}

// Route params reach us straight from the URL, so a malformed percent-escape
// like "100%off" makes decodeURIComponent throw and unwind the render. The raw
// value is the useful fallback: it still identifies the resource to the API.
export function safeDecodeURIComponent(value: string): string {
  try {
    return decodeURIComponent(value)
  } catch {
    return value
  }
}

// The resource part of an ARN is its last "/" segment, or the whole ARN when it
// has none. An absent ARN renders as an empty string.
export function arnResourceId(arn: string | undefined): string {
  return arn?.split("/").at(-1) ?? ""
}

// Case-insensitive comparator for toSorted; a missing name sorts as "".
export function byName<T>(
  getName: (item: T) => string | undefined,
): (a: T, b: T) => number {
  return (a, b) =>
    (getName(a) ?? "").localeCompare(getName(b) ?? "", undefined, {
      sensitivity: "base",
    })
}

export function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) {
    return false
  }

  const setB = new Set(b)

  return a.every((id) => setB.has(id))
}

const TRAILING_SLASH_REGEX = /\/$/

export function removeTrailingSlash(path: string): string {
  return path.replace(TRAILING_SLASH_REGEX, "")
}

export function ensureTrailingSlash(path: string): string {
  if (!path) {
    return path
  }

  return path.endsWith("/") ? path : `${path}/`
}

/**
 * Builds the full S3 key path by combining prefix and key if needed.
 * Handles both relative keys (e.g., "file.txt") and absolute keys (e.g., "folder/file.txt").
 */
export function buildFullS3Key(key: string, currentPrefix: string): string {
  if (key.startsWith(currentPrefix)) {
    return key
  }

  return `${currentPrefix}${key}`
}

/**
 * Extracts the display name from an S3 key by removing the current prefix.
 * Handles both relative keys (already displayable) and absolute keys (need prefix removed).
 */
export function extractDisplayName(key: string, currentPrefix: string): string {
  if (key.startsWith(currentPrefix)) {
    return key.slice(currentPrefix.length)
  }

  return key
}
