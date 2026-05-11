import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getS3Client: () => ({ send: mockSend }),
}))

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

import { useCreateBucket, useDeleteObject, useUploadObject } from "./s3"

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createQueryClient() {
  queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  return queryClient
}

describe("useCreateBucket", () => {
  it("sends CreateBucketCommand with bucket name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateBucket(), { wrapper })

    result.current.mutate({ bucketName: "my-bucket" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Bucket: "my-bucket",
    })
  })

  it("invalidates buckets query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateBucket(), { wrapper })

    result.current.mutate({ bucketName: "my-bucket" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["s3", "buckets"] })
  })
})

describe("useDeleteObject", () => {
  it("sends DeleteObjectCommand with bucket and key", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteObject(), { wrapper })

    result.current.mutate({ bucket: "my-bucket", key: "photos/cat.jpg" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Bucket: "my-bucket",
      Key: "photos/cat.jpg",
    })
  })

  it("invalidates bucket objects query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDeleteObject(), { wrapper })

    result.current.mutate({ bucket: "my-bucket", key: "file.txt" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["s3", "buckets", "my-bucket", "objects"],
    })
  })
})

describe("useUploadObject", () => {
  it("uploads a file and invalidates bucket objects query", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useUploadObject(), { wrapper })

    const file = new File(["hello"], "test.txt", { type: "text/plain" })
    result.current.mutate({ bucket: "my-bucket", key: "test.txt", file })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["s3", "buckets", "my-bucket", "objects"],
    })
  })
})
