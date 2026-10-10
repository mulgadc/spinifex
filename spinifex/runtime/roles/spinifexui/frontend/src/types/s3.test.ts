import { describe, expect, it } from "vitest"

import { createBucketSchema } from "./s3"

describe("createBucketSchema", () => {
  it("accepts a valid bucket name", () => {
    const result = createBucketSchema.safeParse({ bucketName: "my-bucket" })
    expect(result.success).toBeTruthy()
  })

  it("rejects bucket name under 3 characters", () => {
    const result = createBucketSchema.safeParse({ bucketName: "ab" })
    expect(result.success).toBeFalsy()
  })

  it("rejects bucket name over 63 characters", () => {
    const result = createBucketSchema.safeParse({
      bucketName: "a".repeat(64),
    })
    expect(result.success).toBeFalsy()
  })

  it("rejects uppercase letters", () => {
    const result = createBucketSchema.safeParse({ bucketName: "MyBucket" })
    expect(result.success).toBeFalsy()
  })

  it("rejects names starting with a period", () => {
    const result = createBucketSchema.safeParse({ bucketName: ".my-bucket" })
    expect(result.success).toBeFalsy()
  })

  it("rejects names ending with a hyphen", () => {
    const result = createBucketSchema.safeParse({ bucketName: "my-bucket-" })
    expect(result.success).toBeFalsy()
  })

  it("rejects consecutive periods", () => {
    const result = createBucketSchema.safeParse({ bucketName: "my..bucket" })
    expect(result.success).toBeFalsy()
  })

  it("rejects adjacent period and hyphen", () => {
    const result = createBucketSchema.safeParse({ bucketName: "my.-bucket" })
    expect(result.success).toBeFalsy()
  })

  it("rejects adjacent hyphen and period", () => {
    const result = createBucketSchema.safeParse({ bucketName: "my-.bucket" })
    expect(result.success).toBeFalsy()
  })

  it("accepts bucket name with periods and hyphens", () => {
    const result = createBucketSchema.safeParse({
      bucketName: "my.test-bucket.v2",
    })
    expect(result.success).toBeTruthy()
  })

  it("accepts all-numeric bucket name", () => {
    const result = createBucketSchema.safeParse({ bucketName: "123" })
    expect(result.success).toBeTruthy()
  })
})
