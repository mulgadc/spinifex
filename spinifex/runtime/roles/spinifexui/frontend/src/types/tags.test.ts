import { describe, expect, it } from "vitest"

import { tagSchema, toSdkTags } from "./tags"

describe("tagSchema", () => {
  // AWS accepts a key of 1-128 characters and a value of 0-256.
  it.each([
    { key: "k".repeat(128), value: "v".repeat(256), ok: true },
    { key: "k", value: "", ok: true },
    { key: "", value: "v", ok: false },
    { key: "k".repeat(129), value: "v", ok: false },
    { key: "k", value: "v".repeat(257), ok: false },
  ])("enforces the AWS length limits (%#)", ({ key, value, ok }) => {
    expect(tagSchema.safeParse({ key, value }).success).toBe(ok)
  })
})

describe("toSdkTags", () => {
  it("maps rows to the SDK shape and drops blank keys", () => {
    expect(
      toSdkTags([
        { key: "env", value: "prod" },
        { key: "", value: "orphan" },
        { key: "team", value: "" },
      ]),
    ).toStrictEqual([
      { Key: "env", Value: "prod" },
      { Key: "team", Value: "" },
    ])
  })

  it("returns undefined when no keyed rows remain", () => {
    expect(toSdkTags([])).toBeUndefined()
    expect(toSdkTags([{ key: "", value: "" }])).toBeUndefined()
  })
})
