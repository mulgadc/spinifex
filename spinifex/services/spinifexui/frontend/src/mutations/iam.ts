import {
  AddRoleToInstanceProfileCommand,
  AddUserToGroupCommand,
  AttachGroupPolicyCommand,
  AttachRolePolicyCommand,
  AttachUserPolicyCommand,
  CreateAccessKeyCommand,
  CreateGroupCommand,
  CreateInstanceProfileCommand,
  CreatePolicyCommand,
  CreateRoleCommand,
  CreateUserCommand,
  DeleteAccessKeyCommand,
  DeleteGroupCommand,
  DeleteInstanceProfileCommand,
  DeletePolicyCommand,
  DeleteRoleCommand,
  DeleteUserCommand,
  DetachGroupPolicyCommand,
  DetachRolePolicyCommand,
  DetachUserPolicyCommand,
  RemoveRoleFromInstanceProfileCommand,
  RemoveUserFromGroupCommand,
  UpdateAccessKeyCommand,
} from "@aws-sdk/client-iam"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getIamClient } from "@/lib/awsClient"
import type {
  AddRoleToProfileParams,
  CreateGroupFormData,
  CreateInstanceProfileFormData,
  CreatePolicyFormData,
  CreateRoleFormData,
  CreateUserFormData,
  DeleteAccessKeyParams,
  GroupMembershipParams,
  GroupPolicyParams,
  RolePolicyParams,
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
      void queryClient.invalidateQueries({ queryKey: ["iam", "users"] })
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
      void queryClient.invalidateQueries({ queryKey: ["iam", "users"] })
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
      void queryClient.invalidateQueries({ queryKey: ["iam", "access-keys"] })
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
      void queryClient.invalidateQueries({ queryKey: ["iam", "access-keys"] })
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
      void queryClient.invalidateQueries({ queryKey: ["iam", "access-keys"] })
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
      void queryClient.invalidateQueries({ queryKey: ["iam", "policies"] })
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
      void queryClient.invalidateQueries({ queryKey: ["iam", "policies"] })
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
      void queryClient.invalidateQueries({
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
      void queryClient.invalidateQueries({
        queryKey: ["iam", "attached-user-policies"],
      })
    },
  })
}

export function useCreateRole() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateRoleFormData) => {
      const command = new CreateRoleCommand({
        RoleName: params.roleName,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Path: params.path || undefined,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Description: params.description || undefined,
        AssumeRolePolicyDocument: params.assumeRolePolicyDocument,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["iam", "roles"] })
    },
  })
}

export function useDeleteRole() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (roleName: string) => {
      const command = new DeleteRoleCommand({ RoleName: roleName })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["iam", "roles"] })
    },
  })
}

export function useAttachRolePolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ roleName, policyArn }: RolePolicyParams) => {
      const command = new AttachRolePolicyCommand({
        RoleName: roleName,
        PolicyArn: policyArn,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "attached-role-policies"],
      })
    },
  })
}

export function useDetachRolePolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ roleName, policyArn }: RolePolicyParams) => {
      const command = new DetachRolePolicyCommand({
        RoleName: roleName,
        PolicyArn: policyArn,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "attached-role-policies"],
      })
    },
  })
}

export function useCreateInstanceProfile() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateInstanceProfileFormData) => {
      const command = new CreateInstanceProfileCommand({
        InstanceProfileName: params.instanceProfileName,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Path: params.path || undefined,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "instance-profiles"],
      })
    },
  })
}

export function useDeleteInstanceProfile() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (instanceProfileName: string) => {
      const command = new DeleteInstanceProfileCommand({
        InstanceProfileName: instanceProfileName,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "instance-profiles"],
      })
    },
  })
}

export function useAddRoleToInstanceProfile() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      instanceProfileName,
      roleName,
    }: AddRoleToProfileParams) => {
      const command = new AddRoleToInstanceProfileCommand({
        InstanceProfileName: instanceProfileName,
        RoleName: roleName,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "instance-profiles"],
      })
    },
  })
}

export function useRemoveRoleFromInstanceProfile() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      instanceProfileName,
      roleName,
    }: AddRoleToProfileParams) => {
      const command = new RemoveRoleFromInstanceProfileCommand({
        InstanceProfileName: instanceProfileName,
        RoleName: roleName,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "instance-profiles"],
      })
    },
  })
}

export function useCreateGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateGroupFormData) => {
      const command = new CreateGroupCommand({
        GroupName: params.groupName,
        // oxlint-disable-next-line typescript/prefer-nullish-coalescing
        Path: params.path || undefined,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["iam", "groups"] })
    },
  })
}

export function useDeleteGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (groupName: string) => {
      const command = new DeleteGroupCommand({ GroupName: groupName })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["iam", "groups"] })
    },
  })
}

export function useAttachGroupPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ groupName, policyArn }: GroupPolicyParams) => {
      const command = new AttachGroupPolicyCommand({
        GroupName: groupName,
        PolicyArn: policyArn,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "attached-group-policies"],
      })
    },
  })
}

export function useDetachGroupPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ groupName, policyArn }: GroupPolicyParams) => {
      const command = new DetachGroupPolicyCommand({
        GroupName: groupName,
        PolicyArn: policyArn,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", "attached-group-policies"],
      })
    },
  })
}

export function useAddUserToGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ groupName, userName }: GroupMembershipParams) => {
      const command = new AddUserToGroupCommand({
        GroupName: groupName,
        UserName: userName,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["iam", "groups"] })
      void queryClient.invalidateQueries({
        queryKey: ["iam", "groups-for-user"],
      })
    },
  })
}

export function useRemoveUserFromGroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ groupName, userName }: GroupMembershipParams) => {
      const command = new RemoveUserFromGroupCommand({
        GroupName: groupName,
        UserName: userName,
      })
      return await getIamClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["iam", "groups"] })
      void queryClient.invalidateQueries({
        queryKey: ["iam", "groups-for-user"],
      })
    },
  })
}
