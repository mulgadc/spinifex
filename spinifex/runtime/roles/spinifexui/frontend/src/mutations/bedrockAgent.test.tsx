import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { dataSourcesQueryOptions } from "@/queries/bedrockAgent"
import {
  EMPTY_DATA_SOURCE_FORM_DEFAULTS,
  EMPTY_KB_FORM_DEFAULTS,
} from "@/types/bedrockAgent"

import {
  useCreateDataSource,
  useCreateKnowledgeBase,
  useDeleteDataSource,
  useDeleteKnowledgeBase,
} from "./bedrockAgent"

const KB_FORM = {
  ...EMPTY_KB_FORM_DEFAULTS,
  name: "docs-kb",
  embeddingModelArn:
    "arn:aws:bedrock:local::foundation-model/nomic-embed-text-v1.5",
  dimensions: 768,
}

const DATA_SOURCE_FORM = {
  ...EMPTY_DATA_SOURCE_FORM_DEFAULTS,
  name: "s3-docs",
  bucketArn: "arn:aws:s3:::docs-bucket",
}

describe("useCreateKnowledgeBase", () => {
  it("sends CreateKnowledgeBaseCommand with the mapped form values", async () => {
    const { result } = renderMutationHook(() => useCreateKnowledgeBase())

    result.current.mutate(KB_FORM)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.name).toBe("docs-kb")
    expect(input.knowledgeBaseConfiguration.type).toBe("VECTOR")
    expect(input.storageConfiguration).toBeDefined()
  })

  it("invalidates the knowledge base list on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreateKnowledgeBase(),
      queryClient,
    )

    result.current.mutate(KB_FORM)

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock-agent", "knowledge-bases"],
    })
  })
})

describe("useDeleteKnowledgeBase", () => {
  it("sends DeleteKnowledgeBaseCommand with the id", async () => {
    const { result } = renderMutationHook(() => useDeleteKnowledgeBase())

    result.current.mutate("kb-1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
    })
  })

  it("invalidates the list and the single knowledge base on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useDeleteKnowledgeBase(),
      queryClient,
    )

    result.current.mutate("kb-1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock-agent", "knowledge-bases"],
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock-agent", "knowledge-bases", "kb-1"],
    })
  })
})

describe("useCreateDataSource", () => {
  it("sends CreateDataSourceCommand with the knowledge base id and mapped values", async () => {
    const { result } = renderMutationHook(() => useCreateDataSource())

    result.current.mutate({ data: DATA_SOURCE_FORM, knowledgeBaseId: "kb-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    const input = mockSend.mock.calls[0]?.[0].input
    expect(input.knowledgeBaseId).toBe("kb-1")
    expect(input.name).toBe("s3-docs")
    expect(input.dataSourceConfiguration.s3Configuration.bucketArn).toBe(
      "arn:aws:s3:::docs-bucket",
    )
  })

  it("marks the cached data sources for that knowledge base stale on success", async () => {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(dataSourcesQueryOptions("kb-1").queryKey, {
      dataSourceSummaries: [],
      $metadata: {},
    })

    const { result } = renderMutationHook(
      () => useCreateDataSource(),
      queryClient,
    )

    result.current.mutate({ data: DATA_SOURCE_FORM, knowledgeBaseId: "kb-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      queryClient.getQueryState(dataSourcesQueryOptions("kb-1").queryKey)
        ?.isInvalidated,
    ).toBeTruthy()
  })
})

describe("useDeleteDataSource", () => {
  it("sends DeleteDataSourceCommand with both ids", async () => {
    const { result } = renderMutationHook(() => useDeleteDataSource())

    result.current.mutate({ dataSourceId: "ds-1", knowledgeBaseId: "kb-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      dataSourceId: "ds-1",
      knowledgeBaseId: "kb-1",
    })
  })

  it("invalidates the data sources for that knowledge base on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useDeleteDataSource(),
      queryClient,
    )

    result.current.mutate({ dataSourceId: "ds-1", knowledgeBaseId: "kb-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock-agent", "knowledge-bases", "kb-1", "data-sources"],
    })
  })
})
