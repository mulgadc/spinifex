import {
  type ImageIdentifier,
  type ImageTagMutability,
  BatchDeleteImageCommand,
  CreateRepositoryCommand,
  DeleteLifecyclePolicyCommand,
  DeleteRepositoryCommand,
  DeleteRepositoryPolicyCommand,
  GetLifecyclePolicyPreviewCommand,
  PutImageTagMutabilityCommand,
  PutLifecyclePolicyCommand,
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

export interface PutImageTagMutabilityParams {
  repositoryName: string
  imageTagMutability: ImageTagMutability
}

// usePutImageTagMutability flips a repo between MUTABLE and IMMUTABLE; the value
// lives on the repository record, so the repositories list is invalidated.
export function usePutImageTagMutability() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: PutImageTagMutabilityParams) => {
      const command = new PutImageTagMutabilityCommand({
        repositoryName: params.repositoryName,
        imageTagMutability: params.imageTagMutability,
      })
      return await getEcrClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["ecr", "repositories"] })
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

export interface PutLifecyclePolicyParams {
  repositoryName: string
  lifecyclePolicyText: string
}

export function usePutLifecyclePolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: PutLifecyclePolicyParams) => {
      const command = new PutLifecyclePolicyCommand({
        repositoryName: params.repositoryName,
        lifecyclePolicyText: params.lifecyclePolicyText,
      })
      return await getEcrClient().send(command)
    },
    onSuccess: (_data, variables) => {
      void queryClient.invalidateQueries({
        queryKey: [
          "ecr",
          "repositories",
          variables.repositoryName,
          "lifecycle",
        ],
      })
    },
  })
}

export function useDeleteLifecyclePolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (repositoryName: string) => {
      const command = new DeleteLifecyclePolicyCommand({ repositoryName })
      return await getEcrClient().send(command)
    },
    onSuccess: (_data, repositoryName) => {
      void queryClient.invalidateQueries({
        queryKey: ["ecr", "repositories", repositoryName, "lifecycle"],
      })
    },
  })
}

// usePreviewLifecyclePolicy evaluates the repo's saved lifecycle policy against
// its current images without deleting anything. The gateway serves the preview
// synchronously, so the single Get call returns the full result.
export function usePreviewLifecyclePolicy() {
  return useMutation({
    mutationFn: async (repositoryName: string) => {
      const command = new GetLifecyclePolicyPreviewCommand({ repositoryName })
      return await getEcrClient().send(command)
    },
  })
}
