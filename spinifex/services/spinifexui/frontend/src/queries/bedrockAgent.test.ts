import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getBedrockAgentClient: () => ({ send: mockSend }),
}))

import { callQueryFn } from "@/test/query"

import {
  dataSourceQueryOptions,
  dataSourcesQueryOptions,
  ingestionJobQueryOptions,
  ingestionJobsQueryOptions,
  knowledgeBaseQueryOptions,
  knowledgeBasesQueryOptions,
} from "./bedrockAgent"

describe("query keys", () => {
  it("knowledge bases list key", () => {
    expect(knowledgeBasesQueryOptions.queryKey).toStrictEqual([
      "bedrock-agent",
      "knowledge-bases",
    ])
  })

  it("knowledge base key includes id", () => {
    expect(knowledgeBaseQueryOptions("kb-1").queryKey).toStrictEqual([
      "bedrock-agent",
      "knowledge-bases",
      "kb-1",
    ])
  })

  it("data sources key includes knowledge base", () => {
    expect(dataSourcesQueryOptions("kb-1").queryKey).toStrictEqual([
      "bedrock-agent",
      "knowledge-bases",
      "kb-1",
      "data-sources",
    ])
  })

  it("data source key includes knowledge base and data source", () => {
    expect(dataSourceQueryOptions("kb-1", "ds-1").queryKey).toStrictEqual([
      "bedrock-agent",
      "knowledge-bases",
      "kb-1",
      "data-sources",
      "ds-1",
    ])
  })

  it("ingestion jobs key includes knowledge base and data source", () => {
    expect(ingestionJobsQueryOptions("kb-1", "ds-1").queryKey).toStrictEqual([
      "bedrock-agent",
      "knowledge-bases",
      "kb-1",
      "data-sources",
      "ds-1",
      "ingestion-jobs",
    ])
  })

  it("ingestion job key includes job id", () => {
    expect(
      ingestionJobQueryOptions("kb-1", "ds-1", "job-1").queryKey,
    ).toStrictEqual([
      "bedrock-agent",
      "knowledge-bases",
      "kb-1",
      "data-sources",
      "ds-1",
      "ingestion-jobs",
      "job-1",
    ])
  })
})

describe("queryFn", () => {
  beforeEach(() => {
    mockSend.mockClear()
  })

  it("knowledge bases sends ListKnowledgeBasesCommand", async () => {
    await callQueryFn(knowledgeBasesQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("knowledge base sends GetKnowledgeBaseCommand with id", async () => {
    await callQueryFn(knowledgeBaseQueryOptions("kb-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
    })
  })

  it("data sources sends ListDataSourcesCommand with knowledge base", async () => {
    await callQueryFn(dataSourcesQueryOptions("kb-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
    })
  })

  it("data source sends GetDataSourceCommand", async () => {
    await callQueryFn(dataSourceQueryOptions("kb-1", "ds-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
      dataSourceId: "ds-1",
    })
  })

  it("ingestion jobs sends ListIngestionJobsCommand sorted newest first", async () => {
    await callQueryFn(ingestionJobsQueryOptions("kb-1", "ds-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
      dataSourceId: "ds-1",
      sortBy: { attribute: "STARTED_AT", order: "DESCENDING" },
    })
  })

  it("ingestion job sends GetIngestionJobCommand", async () => {
    await callQueryFn(ingestionJobQueryOptions("kb-1", "ds-1", "job-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
      dataSourceId: "ds-1",
      ingestionJobId: "job-1",
    })
  })
})
