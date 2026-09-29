import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getBedrockAgentClient: () => ({ send: mockSend }),
}))

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
    createQueryClient()
    const { result } = renderHook(() => useCreateKnowledgeBase(), { wrapper })

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
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateKnowledgeBase(), { wrapper })

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
    createQueryClient()
    const { result } = renderHook(() => useDeleteKnowledgeBase(), { wrapper })

    result.current.mutate("kb-1")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
    })
  })

  it("invalidates the list and the single knowledge base on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDeleteKnowledgeBase(), { wrapper })

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
    createQueryClient()
    const { result } = renderHook(() => useCreateDataSource(), { wrapper })

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

  it("invalidates the data sources for that knowledge base on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateDataSource(), { wrapper })

    result.current.mutate({ data: DATA_SOURCE_FORM, knowledgeBaseId: "kb-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock-agent", "knowledge-bases", "kb-1", "data-sources"],
    })
  })
})

describe("useDeleteDataSource", () => {
  it("sends DeleteDataSourceCommand with both ids", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteDataSource(), { wrapper })

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
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDeleteDataSource(), { wrapper })

    result.current.mutate({ dataSourceId: "ds-1", knowledgeBaseId: "kb-1" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["bedrock-agent", "knowledge-bases", "kb-1", "data-sources"],
    })
  })
})
