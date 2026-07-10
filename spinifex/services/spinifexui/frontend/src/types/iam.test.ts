import { describe, expect, it } from "vitest"

import {
  createPolicySchema,
  createUserSchema,
  putInlinePolicySchema,
} from "./iam"

describe("createUserSchema", () => {
  it("accepts a valid user name", () => {
    const result = createUserSchema.safeParse({ userName: "admin" })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty user name", () => {
    const result = createUserSchema.safeParse({ userName: "" })
    expect(result.success).toBeFalsy()
  })

  it("rejects user name over 64 chars", () => {
    const result = createUserSchema.safeParse({ userName: "a".repeat(65) })
    expect(result.success).toBeFalsy()
  })

  it("rejects user name with invalid characters", () => {
    const result = createUserSchema.safeParse({ userName: "user name!" })
    expect(result.success).toBeFalsy()
  })

  it("accepts user name with allowed special characters", () => {
    const result = createUserSchema.safeParse({ userName: "user+=,.@-test" })
    expect(result.success).toBeTruthy()
  })

  it("allows optional path", () => {
    const result = createUserSchema.safeParse({
      userName: "admin",
      path: "/engineering/",
    })
    expect(result.success).toBeTruthy()
  })
})

describe("createPolicySchema", () => {
  it("accepts valid policy params", () => {
    const result = createPolicySchema.safeParse({
      policyName: "ReadOnly",
      policyDocument: '{"Version":"2012-10-17","Statement":[]}',
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty policy name", () => {
    const result = createPolicySchema.safeParse({
      policyName: "",
      policyDocument: "{}",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects policy name over 128 chars", () => {
    const result = createPolicySchema.safeParse({
      policyName: "a".repeat(129),
      policyDocument: "{}",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects invalid JSON in policy document", () => {
    const result = createPolicySchema.safeParse({
      policyName: "ReadOnly",
      policyDocument: "not json",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects empty policy document", () => {
    const result = createPolicySchema.safeParse({
      policyName: "ReadOnly",
      policyDocument: "",
    })
    expect(result.success).toBeFalsy()
  })

  it("allows optional description", () => {
    const result = createPolicySchema.safeParse({
      policyName: "ReadOnly",
      description: "Read-only access",
      policyDocument: "{}",
    })
    expect(result.success).toBeTruthy()
  })
})

describe("putInlinePolicySchema", () => {
  it("accepts a valid inline policy", () => {
    const result = putInlinePolicySchema.safeParse({
      policyName: "s3-read",
      policyDocument: '{"Version":"2012-10-17","Statement":[]}',
    })
    expect(result.success).toBeTruthy()
  })

  it("rejects empty policy name", () => {
    const result = putInlinePolicySchema.safeParse({
      policyName: "",
      policyDocument: "{}",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects policy name over 128 chars", () => {
    const result = putInlinePolicySchema.safeParse({
      policyName: "a".repeat(129),
      policyDocument: "{}",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects policy name with invalid characters", () => {
    const result = putInlinePolicySchema.safeParse({
      policyName: "bad name!",
      policyDocument: "{}",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects invalid JSON in policy document", () => {
    const result = putInlinePolicySchema.safeParse({
      policyName: "s3-read",
      policyDocument: "not json",
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects empty policy document", () => {
    const result = putInlinePolicySchema.safeParse({
      policyName: "s3-read",
      policyDocument: "",
    })
    expect(result.success).toBeFalsy()
  })
})
