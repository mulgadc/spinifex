import {
  GetDataSourceCommand,
  GetIngestionJobCommand,
  GetKnowledgeBaseCommand,
  ListDataSourcesCommand,
  ListIngestionJobsCommand,
  ListKnowledgeBasesCommand,
} from "@aws-sdk/client-bedrock-agent"
import { queryOptions } from "@tanstack/react-query"

import { getBedrockAgentClient } from "@/lib/awsClient"

const KNOWLEDGE_BASE_STALE_TIME = 30_000
const INGESTION_JOB_STALE_TIME = 10_000

export const knowledgeBasesQueryOptions = queryOptions({
  queryKey: ["bedrock-agent", "knowledge-bases"],
  queryFn: async () => {
    const command = new ListKnowledgeBasesCommand({})
    return await getBedrockAgentClient().send(command)
  },
  staleTime: KNOWLEDGE_BASE_STALE_TIME,
})

export const knowledgeBaseQueryOptions = (knowledgeBaseId: string) =>
  queryOptions({
    queryKey: ["bedrock-agent", "knowledge-bases", knowledgeBaseId],
    queryFn: async () => {
      const command = new GetKnowledgeBaseCommand({ knowledgeBaseId })
      return await getBedrockAgentClient().send(command)
    },
    staleTime: KNOWLEDGE_BASE_STALE_TIME,
  })

export const dataSourcesQueryOptions = (knowledgeBaseId: string) =>
  queryOptions({
    queryKey: [
      "bedrock-agent",
      "knowledge-bases",
      knowledgeBaseId,
      "data-sources",
    ],
    queryFn: async () => {
      const command = new ListDataSourcesCommand({ knowledgeBaseId })
      return await getBedrockAgentClient().send(command)
    },
    staleTime: KNOWLEDGE_BASE_STALE_TIME,
  })

export const dataSourceQueryOptions = (
  knowledgeBaseId: string,
  dataSourceId: string,
) =>
  queryOptions({
    queryKey: [
      "bedrock-agent",
      "knowledge-bases",
      knowledgeBaseId,
      "data-sources",
      dataSourceId,
    ],
    queryFn: async () => {
      const command = new GetDataSourceCommand({
        knowledgeBaseId,
        dataSourceId,
      })
      return await getBedrockAgentClient().send(command)
    },
    staleTime: KNOWLEDGE_BASE_STALE_TIME,
  })

export const ingestionJobsQueryOptions = (
  knowledgeBaseId: string,
  dataSourceId: string,
) =>
  queryOptions({
    queryKey: [
      "bedrock-agent",
      "knowledge-bases",
      knowledgeBaseId,
      "data-sources",
      dataSourceId,
      "ingestion-jobs",
    ],
    queryFn: async () => {
      const command = new ListIngestionJobsCommand({
        knowledgeBaseId,
        dataSourceId,
        sortBy: { attribute: "STARTED_AT", order: "DESCENDING" },
      })
      return await getBedrockAgentClient().send(command)
    },
    staleTime: INGESTION_JOB_STALE_TIME,
  })

export const ingestionJobQueryOptions = (
  knowledgeBaseId: string,
  dataSourceId: string,
  ingestionJobId: string,
) =>
  queryOptions({
    queryKey: [
      "bedrock-agent",
      "knowledge-bases",
      knowledgeBaseId,
      "data-sources",
      dataSourceId,
      "ingestion-jobs",
      ingestionJobId,
    ],
    queryFn: async () => {
      const command = new GetIngestionJobCommand({
        knowledgeBaseId,
        dataSourceId,
        ingestionJobId,
      })
      return await getBedrockAgentClient().send(command)
    },
    staleTime: INGESTION_JOB_STALE_TIME,
  })
