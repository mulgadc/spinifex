import {
  type ImageIdentifier,
  BatchDeleteImageCommand,
  CreateRepositoryCommand,
  DeleteRepositoryCommand,
  DeleteRepositoryPolicyCommand,
  SetRepositoryPolicyCommand,
} from "@aws-sdk/client-ecr"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getEcrClient } from "@/lib/awsClient"

export function useCreateRepository() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (repositoryName: string) => {
      const command = new CreateRepositoryCommand({ repositoryName })
      return await getEcrClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ecr", "repositories"] })
    },
  })
}

// useDeleteRepository force-deletes a repo and every image it holds; AWS rejects
// a non-empty repo without force, so force is always set from the console.
export function useDeleteRepository() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (repositoryName: string) => {
      const command = new DeleteRepositoryCommand({
        repositoryName,
        force: true,
      })
      return await getEcrClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ecr", "repositories"] })
    },
  })
}

export interface BatchDeleteImageParams {
  repositoryName: string
  imageIds: ImageIdentifier[]
}

export function useBatchDeleteImage() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: BatchDeleteImageParams) => {
      const command = new BatchDeleteImageCommand({
        repositoryName: params.repositoryName,
        imageIds: params.imageIds,
      })
      return await getEcrClient().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecr", "repositories", variables.repositoryName, "images"],
      })
    },
  })
}

export interface SetRepositoryPolicyParams {
  repositoryName: string
  policyText: string
}

export function useSetRepositoryPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: SetRepositoryPolicyParams) => {
      const command = new SetRepositoryPolicyCommand({
        repositoryName: params.repositoryName,
        policyText: params.policyText,
      })
      return await getEcrClient().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecr", "repositories", variables.repositoryName, "policy"],
      })
    },
  })
}

export function useDeleteRepositoryPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (repositoryName: string) => {
      const command = new DeleteRepositoryPolicyCommand({ repositoryName })
      return await getEcrClient().send(command)
    },
    onSuccess: (_data, repositoryName) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecr", "repositories", repositoryName, "policy"],
      })
    },
  })
}
