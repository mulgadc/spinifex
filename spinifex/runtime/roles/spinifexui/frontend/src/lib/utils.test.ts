import { describe, expect, it } from "vitest"

import {
  arnResourceId,
  buildFullS3Key,
  byName,
  cn,
  ensureTrailingSlash,
  extractDisplayName,
  formatDateTime,
  formatSize,
  getNameTag,
  removeTrailingSlash,
  safeDecodeURIComponent,
  sameSet,
  vpcLabel,
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

  it("treats custom text size tokens as font sizes, not colors", () => {
    expect(cn("text-code-inline", "text-muted-foreground")).toBe(
      "text-code-inline text-muted-foreground",
    )
    expect(cn("text-2xs", "text-code-block")).toBe("text-code-block")
  })

  it("returns empty string with no inputs", () => {
    expect(cn()).toBe("")
  })
})

describe("formatDateTime", () => {
  it("matches the AWS console's form with a whole-hour offset", () => {
    expect(
      formatDateTime(new Date("2026-06-04T07:00:00Z"), "America/Chicago"),
    ).toBe("2026/06/04 02:00 GMT-5")
  })

  it("keeps a half-hour offset and a 24-hour clock", () => {
    expect(formatDateTime("2026-01-01T13:05:00Z", "Australia/Adelaide")).toBe(
      "2026/01/01 23:35 GMT+10:30",
    )
  })

  it("names UTC as plain GMT", () => {
    expect(formatDateTime("2026-01-01T00:00:00Z", "UTC")).toBe(
      "2026/01/01 00:00 GMT",
    )
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

describe("arnResourceId", () => {
  it("returns the segment after the last slash", () => {
    expect(
      arnResourceId(
        "arn:aws:ecs:us-east-1:123456789012:task/my-cluster/0a1b2c3d",
      ),
    ).toBe("0a1b2c3d")
  })

  it("keeps the family:revision of a task definition", () => {
    expect(
      arnResourceId("arn:aws:ecs:us-east-1:123456789012:task-definition/web:7"),
    ).toBe("web:7")
  })

  it("returns the whole ARN when it has no slash", () => {
    expect(arnResourceId("arn:aws:sns:us-east-1:123456789012:topic")).toBe(
      "arn:aws:sns:us-east-1:123456789012:topic",
    )
  })

  it("returns an empty string for a missing ARN", () => {
    expect(arnResourceId(undefined)).toBe("")
  })
})

describe("byName", () => {
  it("orders case-insensitively", () => {
    const sorted = [{ n: "charlie" }, { n: "Bravo" }, { n: "alpha" }].toSorted(
      byName((x) => x.n),
    )

    expect(sorted.map((x) => x.n)).toStrictEqual(["alpha", "Bravo", "charlie"])
  })

  it("sorts a missing name first, as an empty string", () => {
    const sorted = [{ n: "alpha" }, { n: undefined }].toSorted(
      byName((x) => x.n),
    )

    expect(sorted.map((x) => x.n)).toStrictEqual([undefined, "alpha"])
  })

  it("keeps names differing only in case in their original order", () => {
    const sorted = [{ n: "WEB" }, { n: "web" }].toSorted(byName((x) => x.n))

    expect(sorted.map((x) => x.n)).toStrictEqual(["WEB", "web"])
  })
})

describe("sameSet", () => {
  it("treats the same members in a different order as equal", () => {
    expect(sameSet(["sg-1", "sg-2"], ["sg-2", "sg-1"])).toBeTruthy()
  })

  it("rejects lists of different sizes", () => {
    expect(sameSet(["sg-1"], ["sg-1", "sg-2"])).toBeFalsy()
  })

  it("rejects lists of the same size with different members", () => {
    expect(sameSet(["sg-1", "sg-2"], ["sg-1", "sg-3"])).toBeFalsy()
  })
})

describe("vpcLabel", () => {
  it("names the VPC by its Name tag", () => {
    expect(
      vpcLabel({
        VpcId: "vpc-1",
        CidrBlock: "10.0.0.0/16",
        Tags: [{ Key: "Name", Value: "prod" }],
      }),
    ).toBe("vpc-1 (prod)")
  })

  it("falls back to the CIDR when the VPC has no usable Name tag", () => {
    expect(vpcLabel({ VpcId: "vpc-1", CidrBlock: "10.0.0.0/16" })).toBe(
      "vpc-1 (10.0.0.0/16)",
    )
    expect(
      vpcLabel({
        VpcId: "vpc-1",
        CidrBlock: "10.0.0.0/16",
        Tags: [{ Key: "Name", Value: "" }],
      }),
    ).toBe("vpc-1 (10.0.0.0/16)")
  })
})
