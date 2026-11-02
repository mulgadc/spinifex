import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import {
  s3BucketObjectsQueryOptions,
  s3BucketsQueryOptions,
} from "@/queries/s3"

import {
  useCreateBucket,
  useDeleteBucket,
  useDeleteObject,
  useDeleteObjects,
} from "./s3"

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

interface SentCommand {
  constructor: { name: string }
  input: {
    Prefix?: string
    ContinuationToken?: string
    Delete?: { Objects: { Key: string }[] }
  }
}

function sent(name: string): SentCommand["input"][] {
  return mockSend.mock.calls.flatMap(([command]: SentCommand[]) =>
    command?.constructor.name === name ? [command.input] : [],
  )
}

describe("useDeleteBucket", () => {
  it("sends DeleteBucketCommand and marks the bucket list stale", async () => {
    mockSend.mockClear()
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(s3BucketsQueryOptions.queryKey, { $metadata: {} })
    const { result } = renderMutationHook(() => useDeleteBucket(), queryClient)

    result.current.mutate("old-bucket")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(sent("DeleteBucketCommand")).toStrictEqual([
      { Bucket: "old-bucket" },
    ])
    expect(
      queryClient.getQueryState(s3BucketsQueryOptions.queryKey)?.isInvalidated,
    ).toBeTruthy()
  })
})

describe("useDeleteObjects", () => {
  it("deletes a folder with every key under it, across listing pages", async () => {
    mockSend.mockReset()
    mockSend.mockImplementation(async (command: SentCommand) => {
      if (command.constructor.name !== "ListObjectsV2Command") {
        return {}
      }

      return command.input.ContinuationToken
        ? { Contents: [{ Key: "logs/b" }], IsTruncated: false }
        : {
            Contents: [{ Key: "logs/a" }],
            IsTruncated: true,
            NextContinuationToken: "next",
          }
    })
    const { result } = renderMutationHook(() => useDeleteObjects("bkt"))

    const failed = await result.current(["readme.md", "logs/"])

    expect(failed.size).toBe(0)
    expect(
      sent("ListObjectsV2Command").map((input) => input.Prefix),
    ).toStrictEqual(["logs/", "logs/"])
    expect(
      sent("DeleteObjectsCommand").map((input) =>
        input.Delete?.Objects.map(({ Key }) => Key),
      ),
    ).toStrictEqual([["readme.md"], ["logs/a", "logs/b"]])
  })

  it("splits more than 1000 keys into batches S3 accepts", async () => {
    const keys = Array.from({ length: 1001 }, (_, index) => `k${index}`)
    mockSend.mockReset()
    mockSend.mockImplementation(async (command: SentCommand) =>
      command.constructor.name === "ListObjectsV2Command"
        ? { Contents: keys.map((Key) => ({ Key })) }
        : {},
    )
    const { result } = renderMutationHook(() => useDeleteObjects("bkt"))

    await result.current(["big/"])

    expect(
      sent("DeleteObjectsCommand").map((input) => input.Delete?.Objects.length),
    ).toStrictEqual([1000, 1])
  })

  it("names each refused key, and a folder by how much of it was left", async () => {
    mockSend.mockReset()
    mockSend.mockImplementation(async (command: SentCommand) => {
      if (command.constructor.name === "ListObjectsV2Command") {
        return { Contents: [{ Key: "logs/a" }, { Key: "logs/b" }] }
      }

      return {
        Errors: (command.input.Delete?.Objects ?? [])
          .filter(({ Key }) => Key !== "logs/b")
          .map(({ Key }) => ({ Key, Message: "Access Denied" })),
      }
    })
    const { result } = renderMutationHook(() => useDeleteObjects("bkt"))

    const failed = await result.current(["readme.md", "logs/"])

    expect([...failed]).toStrictEqual([
      ["readme.md", "Access Denied"],
      ["logs/", "1 of 2 objects were not deleted: Access Denied"],
    ])
  })
})
