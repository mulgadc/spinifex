import { describe, expect, it } from "vitest"

import { diffTags, tagSchema, toSdkTags } from "./tags"

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

describe("diffTags", () => {
  it("sets the whole desired set and removes only the keys that went away", () => {
    expect(
      diffTags(
        [
          { key: "env", value: "staging" },
          { key: "owner", value: "alice" },
        ],
        ["env", "team"],
      ),
    ).toStrictEqual({
      toSet: [
        { Key: "env", Value: "staging" },
        { Key: "owner", Value: "alice" },
      ],
      toRemove: ["team"],
    })
  })

  it("removes every prior key when the desired set is empty", () => {
    expect(diffTags([], ["env", "team"])).toStrictEqual({
      toSet: undefined,
      toRemove: ["env", "team"],
    })
  })
})
