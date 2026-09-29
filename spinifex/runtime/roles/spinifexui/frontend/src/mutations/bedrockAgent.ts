import {
  CreateDataSourceCommand,
  CreateKnowledgeBaseCommand,
  DeleteDataSourceCommand,
  DeleteKnowledgeBaseCommand,
} from "@aws-sdk/client-bedrock-agent"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getBedrockAgentClient } from "@/lib/awsClient"
import {
  type DataSourceFormData,
  formToCreateDataSourceInput,
  formToCreateKnowledgeBaseInput,
  type KnowledgeBaseFormData,
} from "@/types/bedrockAgent"

const KNOWLEDGE_BASES_KEY = ["bedrock-agent", "knowledge-bases"]

function knowledgeBaseKey(knowledgeBaseId: string) {
  return [...KNOWLEDGE_BASES_KEY, knowledgeBaseId]
}

function dataSourcesKey(knowledgeBaseId: string) {
  return [...knowledgeBaseKey(knowledgeBaseId), "data-sources"]
}

export function useCreateKnowledgeBase() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (data: KnowledgeBaseFormData) => {
      const command = new CreateKnowledgeBaseCommand(
        formToCreateKnowledgeBaseInput(data),
      )
      return await getBedrockAgentClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: KNOWLEDGE_BASES_KEY })
    },
  })
}

export function useDeleteKnowledgeBase() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (knowledgeBaseId: string) => {
      const command = new DeleteKnowledgeBaseCommand({ knowledgeBaseId })
      return await getBedrockAgentClient().send(command)
    },
    onSuccess: (_data, knowledgeBaseId) => {
      void queryClient.invalidateQueries({ queryKey: KNOWLEDGE_BASES_KEY })
      void queryClient.invalidateQueries({
        queryKey: knowledgeBaseKey(knowledgeBaseId),
      })
    },
  })
}

export interface CreateDataSourceParams {
  knowledgeBaseId: string
  data: DataSourceFormData
}

export function useCreateDataSource() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ knowledgeBaseId, data }: CreateDataSourceParams) => {
      const command = new CreateDataSourceCommand(
        formToCreateDataSourceInput(knowledgeBaseId, data),
      )
      return await getBedrockAgentClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: dataSourcesKey(params.knowledgeBaseId),
      })
    },
  })
}

export interface DeleteDataSourceParams {
  knowledgeBaseId: string
  dataSourceId: string
}

export function useDeleteDataSource() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      knowledgeBaseId,
      dataSourceId,
    }: DeleteDataSourceParams) => {
      const command = new DeleteDataSourceCommand({
        knowledgeBaseId,
        dataSourceId,
      })
      return await getBedrockAgentClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: dataSourcesKey(params.knowledgeBaseId),
      })
    },
  })
}
