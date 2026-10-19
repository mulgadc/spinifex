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
  DeleteGroupPolicyCommand,
  DeleteInstanceProfileCommand,
  DeletePolicyCommand,
  DeleteRoleCommand,
  DeleteRolePolicyCommand,
  DeleteUserCommand,
  DeleteUserPolicyCommand,
  DetachGroupPolicyCommand,
  DetachRolePolicyCommand,
  DetachUserPolicyCommand,
  PutGroupPolicyCommand,
  PutRolePolicyCommand,
  PutUserPolicyCommand,
  RemoveRoleFromInstanceProfileCommand,
  RemoveUserFromGroupCommand,
  UpdateAccessKeyCommand,
} from "@aws-sdk/client-iam"
import {
  type QueryClient,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query"

import { getIamClient } from "@/lib/awsClient"
import type {
  AddRoleToProfileParams,
  CreateGroupFormData,
  CreateInstanceProfileFormData,
  CreatePolicyFormData,
  CreateRoleFormData,
  CreateUserFormData,
  DeleteAccessKeyParams,
  DeleteInlinePolicyParams,
  GroupMembershipParams,
  IamPrincipalKind,
  PrincipalPolicyParams,
  PutInlinePolicyParams,
  UpdateAccessKeyParams,
} from "@/types/iam"

type PrincipalSenders<TParams> = Record<
  IamPrincipalKind,
  (params: TParams) => Promise<void>
>

export function useCreateUser() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async (params: CreateUserFormData) => {
      const command = new CreateUserCommand({
        UserName: params.userName,
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

// One sender per principal kind keeps each SDK command's input type checked.
const attachPolicy = {
  user: async ({ name, policyArn }) => {
    await getIamClient().send(
      new AttachUserPolicyCommand({ UserName: name, PolicyArn: policyArn }),
    )
  },
  role: async ({ name, policyArn }) => {
    await getIamClient().send(
      new AttachRolePolicyCommand({ RoleName: name, PolicyArn: policyArn }),
    )
  },
  group: async ({ name, policyArn }) => {
    await getIamClient().send(
      new AttachGroupPolicyCommand({ GroupName: name, PolicyArn: policyArn }),
    )
  },
} satisfies PrincipalSenders<PrincipalPolicyParams>

const detachPolicy = {
  user: async ({ name, policyArn }) => {
    await getIamClient().send(
      new DetachUserPolicyCommand({ UserName: name, PolicyArn: policyArn }),
    )
  },
  role: async ({ name, policyArn }) => {
    await getIamClient().send(
      new DetachRolePolicyCommand({ RoleName: name, PolicyArn: policyArn }),
    )
  },
  group: async ({ name, policyArn }) => {
    await getIamClient().send(
      new DetachGroupPolicyCommand({ GroupName: name, PolicyArn: policyArn }),
    )
  },
} satisfies PrincipalSenders<PrincipalPolicyParams>

const putInlinePolicy = {
  user: async ({ name, policyName, policyDocument }) => {
    await getIamClient().send(
      new PutUserPolicyCommand({
        UserName: name,
        PolicyName: policyName,
        PolicyDocument: policyDocument,
      }),
    )
  },
  role: async ({ name, policyName, policyDocument }) => {
    await getIamClient().send(
      new PutRolePolicyCommand({
        RoleName: name,
        PolicyName: policyName,
        PolicyDocument: policyDocument,
      }),
    )
  },
  group: async ({ name, policyName, policyDocument }) => {
    await getIamClient().send(
      new PutGroupPolicyCommand({
        GroupName: name,
        PolicyName: policyName,
        PolicyDocument: policyDocument,
      }),
    )
  },
} satisfies PrincipalSenders<PutInlinePolicyParams>

const deleteInlinePolicy = {
  user: async ({ name, policyName }) => {
    await getIamClient().send(
      new DeleteUserPolicyCommand({ UserName: name, PolicyName: policyName }),
    )
  },
  role: async ({ name, policyName }) => {
    await getIamClient().send(
      new DeleteRolePolicyCommand({ RoleName: name, PolicyName: policyName }),
    )
  },
  group: async ({ name, policyName }) => {
    await getIamClient().send(
      new DeleteGroupPolicyCommand({ GroupName: name, PolicyName: policyName }),
    )
  },
} satisfies PrincipalSenders<DeleteInlinePolicyParams>

// Attached-policy keys carry no name, so one attach refreshes every principal
// page of that kind; inline keys are per principal.
export function useAttachPolicy(kind: IamPrincipalKind) {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: attachPolicy[kind],
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", `attached-${kind}-policies`],
      })
    },
  })
}

export function useDetachPolicy(kind: IamPrincipalKind) {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: detachPolicy[kind],
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", `attached-${kind}-policies`],
      })
    },
  })
}

export function usePutInlinePolicy(kind: IamPrincipalKind) {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: putInlinePolicy[kind],
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", `${kind}-inline-policies`, name],
      })
    },
  })
}

export function useDeleteInlinePolicy(kind: IamPrincipalKind) {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: deleteInlinePolicy[kind],
    onSuccess: (_data, { name }) => {
      void queryClient.invalidateQueries({
        queryKey: ["iam", `${kind}-inline-policies`, name],
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
        Path: params.path || undefined,
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

export function useCreateInstanceProfile() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async (params: CreateInstanceProfileFormData) => {
      const command = new CreateInstanceProfileCommand({
        InstanceProfileName: params.instanceProfileName,
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

// Profile membership changes also change what each role page lists.
function invalidateInstanceProfiles(queryClient: QueryClient) {
  void queryClient.invalidateQueries({ queryKey: ["iam", "instance-profiles"] })
  void queryClient.invalidateQueries({
    queryKey: ["iam", "instance-profiles-for-role"],
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
      invalidateInstanceProfiles(queryClient)
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
      invalidateInstanceProfiles(queryClient)
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
      invalidateInstanceProfiles(queryClient)
    },
  })
}

export function useCreateGroup() {
  const queryClient = useQueryClient()

  return useMutation({
    mutationFn: async (params: CreateGroupFormData) => {
      const command = new CreateGroupCommand({
        GroupName: params.groupName,
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
