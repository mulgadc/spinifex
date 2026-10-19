import { describe, expect, it, vi } from "vitest"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { callQueryFn } from "@/test/query"

import {
  dataSourcesQueryOptions,
  ingestionJobsQueryOptions,
  knowledgeBaseQueryOptions,
  knowledgeBasesQueryOptions,
} from "./bedrockAgent"

describe("queryFn", () => {
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

  it("ingestion jobs sends ListIngestionJobsCommand sorted newest first", async () => {
    await callQueryFn(ingestionJobsQueryOptions("kb-1", "ds-1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      knowledgeBaseId: "kb-1",
      dataSourceId: "ds-1",
      sortBy: { attribute: "STARTED_AT", order: "DESCENDING" },
    })
  })
})
