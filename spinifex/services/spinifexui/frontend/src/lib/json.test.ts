import { describe, expect, it } from "vitest"

import { isValidJson, jsonStringSchema } from "./json"

type JsonSchema = ReturnType<typeof jsonStringSchema>

function issueMessages(schema: JsonSchema, value: string): string[] {
  const result = schema.safeParse(value)
  return result.success ? [] : result.error.issues.map((issue) => issue.message)
}

describe("isValidJson", () => {
  it("accepts valid JSON", () => {
    expect(isValidJson('{"a":1}')).toBeTruthy()
    expect(isValidJson("[]")).toBeTruthy()
    expect(isValidJson('"str"')).toBeTruthy()
  })

  it("rejects malformed JSON and empty strings", () => {
    expect(isValidJson("{")).toBeFalsy()
    expect(isValidJson("{a:1}")).toBeFalsy()
    expect(isValidJson("")).toBeFalsy()
  })
})

describe("jsonStringSchema", () => {
  const required = jsonStringSchema({ label: "Policy document" })
  const optional = jsonStringSchema({
    label: "Configuration",
    allowEmpty: true,
  })

  it("accepts valid JSON", () => {
    expect(required.safeParse('{"Version":"2012-10-17"}').success).toBeTruthy()
  })

  it("rejects malformed JSON with a derived message", () => {
    expect(issueMessages(required, "{ not json")).toContain(
      "Policy document must be valid JSON",
    )
  })

  it("rejects empty input when required", () => {
    expect(issueMessages(required, "")).toContain("Policy document is required")
  })

  it("allows empty input when allowEmpty is set", () => {
    expect(optional.safeParse("").success).toBeTruthy()
    expect(optional.safeParse("   ").success).toBeTruthy()
  })

  it("still rejects malformed input when allowEmpty is set", () => {
    expect(optional.safeParse("{bad").success).toBeFalsy()
  })
})
