import { describe, expect, it } from "vitest"

import {
  buildFullS3Key,
  cn,
  ensureTrailingSlash,
  extractDisplayName,
  formatDateTime,
  formatSize,
  getNameTag,
  removeTrailingSlash,
  safeDecodeURIComponent,
} from "./utils"

describe("cn", () => {
  it("merges class names", () => {
    expect(cn("foo", "bar")).toBe("foo bar")
  })

  it("handles conditional classes", () => {
    expect(cn("foo", false, "baz")).toBe("foo baz")
  })

  it("deduplicates conflicting tailwind classes", () => {
    expect(cn("p-4", "p-2")).toBe("p-2")
  })

  it("returns empty string with no inputs", () => {
    expect(cn()).toBe("")
  })
})

describe("formatDateTime", () => {
  it("formats a Date object", () => {
    const date = new Date("2025-06-15T10:30:00Z")
    const result = formatDateTime(date)
    expect(result).toContain("2025")
    expect(result).toContain("15")
  })

  it("formats an ISO string", () => {
    const result = formatDateTime("2025-01-01T00:00:00Z")
    expect(result).toContain("2025")
  })

  it("returns 'Unknown' for undefined", () => {
    expect(formatDateTime(undefined)).toBe("Unknown")
  })

  it("returns 'Unknown' for empty string", () => {
    expect(formatDateTime("")).toBe("Unknown")
  })
})

describe("formatSize", () => {
  it("returns '0 B' for zero bytes", () => {
    expect(formatSize(0)).toBe("0 B")
  })

  it("formats bytes", () => {
    expect(formatSize(500)).toBe("500.00 B")
  })

  it("formats kilobytes", () => {
    expect(formatSize(1024)).toBe("1.00 KB")
    expect(formatSize(1536)).toBe("1.50 KB")
  })

  it("formats megabytes", () => {
    expect(formatSize(1024 * 1024)).toBe("1.00 MB")
  })

  it("formats gigabytes", () => {
    expect(formatSize(1024 ** 3)).toBe("1.00 GB")
  })

  it("formats terabytes", () => {
    expect(formatSize(1024 ** 4)).toBe("1.00 TB")
  })
})

describe("getNameTag", () => {
  it("returns the Name tag value", () => {
    const tags = [
      { Key: "Env", Value: "prod" },
      { Key: "Name", Value: "my-instance" },
    ]
    expect(getNameTag(tags)).toBe("my-instance")
  })

  it("returns undefined when Name tag is missing", () => {
    const tags = [{ Key: "Env", Value: "prod" }]
    expect(getNameTag(tags)).toBeUndefined()
  })

  it("returns undefined for empty array", () => {
    expect(getNameTag([])).toBeUndefined()
  })

  it("returns undefined for undefined input", () => {
    expect(getNameTag(undefined)).toBeUndefined()
  })
})

describe("removeTrailingSlash", () => {
  it("removes trailing slash", () => {
    expect(removeTrailingSlash("foo/")).toBe("foo")
  })

  it("leaves paths without trailing slash unchanged", () => {
    expect(removeTrailingSlash("foo")).toBe("foo")
  })

  it("handles root slash", () => {
    expect(removeTrailingSlash("/")).toBe("")
  })

  it("handles empty string", () => {
    expect(removeTrailingSlash("")).toBe("")
  })

  it("only removes the last slash", () => {
    expect(removeTrailingSlash("foo/bar/")).toBe("foo/bar")
  })
})

describe("ensureTrailingSlash", () => {
  it("adds trailing slash when missing", () => {
    expect(ensureTrailingSlash("foo")).toBe("foo/")
  })

  it("does not double slash", () => {
    expect(ensureTrailingSlash("foo/")).toBe("foo/")
  })

  it("returns empty string as-is", () => {
    expect(ensureTrailingSlash("")).toBe("")
  })
})

describe("buildFullS3Key", () => {
  it("returns key as-is when it already starts with prefix", () => {
    expect(buildFullS3Key("photos/cat.jpg", "photos/")).toBe("photos/cat.jpg")
  })

  it("prepends prefix to relative key", () => {
    expect(buildFullS3Key("cat.jpg", "photos/")).toBe("photos/cat.jpg")
  })

  it("handles empty prefix", () => {
    expect(buildFullS3Key("cat.jpg", "")).toBe("cat.jpg")
  })
})

describe("extractDisplayName", () => {
  it("strips prefix from absolute key", () => {
    expect(extractDisplayName("photos/cat.jpg", "photos/")).toBe("cat.jpg")
  })

  it("returns relative key unchanged", () => {
    expect(extractDisplayName("cat.jpg", "photos/")).toBe("cat.jpg")
  })

  it("handles empty prefix", () => {
    expect(extractDisplayName("cat.jpg", "")).toBe("cat.jpg")
  })

  it("handles nested prefixes", () => {
    expect(extractDisplayName("a/b/c/file.txt", "a/b/")).toBe("c/file.txt")
  })
})

describe("safeDecodeURIComponent", () => {
  it("decodes a well-formed value", () => {
    expect(safeDecodeURIComponent("arn%3Aaws%3Ards%3Adb%2Fmine")).toBe(
      "arn:aws:rds:db/mine",
    )
  })

  it("returns the raw value on a lone percent", () => {
    expect(safeDecodeURIComponent("100%off")).toBe("100%off")
  })

  it("returns the raw value on a truncated escape", () => {
    expect(safeDecodeURIComponent("db-%E0%A4%A")).toBe("db-%E0%A4%A")
  })

  it("leaves an undecorated value untouched", () => {
    expect(safeDecodeURIComponent("my-db-instance")).toBe("my-db-instance")
  })
})
