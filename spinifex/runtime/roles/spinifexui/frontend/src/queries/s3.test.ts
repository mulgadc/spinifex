import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { callQueryFn } from "@/test/query"

import { s3BucketObjectsQueryOptions, s3BucketsQueryOptions } from "./s3"

describe("queryFn", () => {
  it("s3BucketsQueryOptions sends ListBucketsCommand", async () => {
    await callQueryFn(s3BucketsQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("s3BucketObjectsQueryOptions sends ListObjectsV2Command with bucket and delimiter", async () => {
    await callQueryFn(s3BucketObjectsQueryOptions("my-bucket"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Bucket: "my-bucket",
      Prefix: undefined,
      Delimiter: "/",
    })
  })

  it("s3BucketObjectsQueryOptions sends prefix when provided", async () => {
    await callQueryFn(s3BucketObjectsQueryOptions("my-bucket", "docs/"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Bucket: "my-bucket",
      Prefix: "docs/",
      Delimiter: "/",
    })
  })
})
