import { type ClassValue, clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs))
}

const dateFormatter = new Intl.DateTimeFormat(undefined, {
  year: "numeric",
  month: "long",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
})

export function formatDateTime(date: Date | string | undefined): string {
  if (!date) {
    return "Unknown"
  }

  return dateFormatter.format(new Date(date))
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
