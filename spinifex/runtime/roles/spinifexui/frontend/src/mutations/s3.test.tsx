import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

vi.mock("@aws-sdk/lib-storage", () => ({
  Upload: class MockUpload {
    private readonly key: string
    constructor({ params }: { params: { Key: string } }) {
      this.key = params.Key
    }
    async done() {
      return { Key: this.key }
    }
  },
}))

import {
  s3BucketObjectsQueryOptions,
  s3BucketsQueryOptions,
} from "@/queries/s3"

import { useCreateBucket, useDeleteObject, useUploadObject } from "./s3"

describe("useCreateBucket", () => {
  it("sends CreateBucketCommand with bucket name", async () => {
    const { result } = renderMutationHook(() => useCreateBucket())

    result.current.mutate({ bucketName: "my-bucket" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Bucket: "my-bucket",
    })
  })

  it("marks the cached bucket list stale on success", async () => {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(s3BucketsQueryOptions.queryKey, { $metadata: {} })
    const { result } = renderMutationHook(() => useCreateBucket(), queryClient)

    result.current.mutate({ bucketName: "my-bucket" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      queryClient.getQueryState(s3BucketsQueryOptions.queryKey)?.isInvalidated,
    ).toBeTruthy()
  })
})

describe("useDeleteObject", () => {
  it("sends DeleteObjectCommand with bucket and key", async () => {
    const { result } = renderMutationHook(() => useDeleteObject())

    result.current.mutate({ bucket: "my-bucket", key: "photos/cat.jpg" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Bucket: "my-bucket",
      Key: "photos/cat.jpg",
    })
  })

  it("marks every cached listing of the bucket stale on success", async () => {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(
      s3BucketObjectsQueryOptions("my-bucket").queryKey,
      { $metadata: {} },
    )
    queryClient.setQueryData(
      s3BucketObjectsQueryOptions("my-bucket", "photos/").queryKey,
      { $metadata: {} },
    )
    const { result } = renderMutationHook(() => useDeleteObject(), queryClient)

    result.current.mutate({ bucket: "my-bucket", key: "file.txt" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      queryClient.getQueryState(
        s3BucketObjectsQueryOptions("my-bucket").queryKey,
      )?.isInvalidated,
    ).toBeTruthy()
    expect(
      queryClient.getQueryState(
        s3BucketObjectsQueryOptions("my-bucket", "photos/").queryKey,
      )?.isInvalidated,
    ).toBeTruthy()
  })
})

describe("useUploadObject", () => {
  it("uploads a file and invalidates bucket objects query", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderMutationHook(() => useUploadObject(), queryClient)

    const file = new File(["hello"], "test.txt", { type: "text/plain" })
    result.current.mutate({ bucket: "my-bucket", key: "test.txt", file })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["s3", "buckets", "my-bucket", "objects"],
    })
  })
})
