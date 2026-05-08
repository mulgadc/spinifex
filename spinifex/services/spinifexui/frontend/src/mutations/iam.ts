import {
  AttachUserPolicyCommand,
  CreateAccessKeyCommand,
  CreatePolicyCommand,
  CreateUserCommand,
  DeleteAccessKeyCommand,
  DeletePolicyCommand,
  DeleteUserCommand,
  DetachUserPolicyCommand,
  UpdateAccessKeyCommand,
} from "@aws-sdk/client-iam"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getIamClient } from "@/lib/awsClient"
import type {
  CreatePolicyFormData,
  CreateUserFormData,
  DeleteAccessKeyParams,
  UpdateAccessKeyParams,
  UserPolicyParams,
} from "@/types/iam"

export function useCreateUser() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateUserFormData) => {
      const command = new CreateUserCommand({
        UserName: params.userName,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Path: params.path || undefined,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["iam", "users"] })
    },
  })
}

export function useDeleteUser() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (userName: string) => {
      const command = new DeleteUserCommand({ UserName: userName })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["iam", "users"] })
    },
  })
}

export function useCreateAccessKey() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (userName: string) => {
      const command = new CreateAccessKeyCommand({ UserName: userName })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["iam", "access-keys"] })
    },
  })
}

export function useDeleteAccessKey() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ userName, accessKeyId }: DeleteAccessKeyParams) => {
      const command = new DeleteAccessKeyCommand({
        UserName: userName,
        AccessKeyId: accessKeyId,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["iam", "access-keys"] })
    },
  })
}

export function useUpdateAccessKey() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      userName,
      accessKeyId,
      status,
    }: UpdateAccessKeyParams) => {
      const command = new UpdateAccessKeyCommand({
        UserName: userName,
        AccessKeyId: accessKeyId,
        Status: status,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["iam", "access-keys"] })
    },
  })
}

export function useCreatePolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreatePolicyFormData) => {
      const command = new CreatePolicyCommand({
        PolicyName: params.policyName,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Description: params.description || undefined,
        PolicyDocument: params.policyDocument,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["iam", "policies"] })
    },
  })
}

export function useDeletePolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (policyArn: string) => {
      const command = new DeletePolicyCommand({ PolicyArn: policyArn })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["iam", "policies"] })
    },
  })
}

export function useAttachUserPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ userName, policyArn }: UserPolicyParams) => {
      const command = new AttachUserPolicyCommand({
        UserName: userName,
        PolicyArn: policyArn,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: ["iam", "attached-user-policies"],
      })
    },
  })
}

export function useDetachUserPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ userName, policyArn }: UserPolicyParams) => {
      const command = new DetachUserPolicyCommand({
        UserName: userName,
        PolicyArn: policyArn,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: ["iam", "attached-user-policies"],
      })
    },
  })
}
