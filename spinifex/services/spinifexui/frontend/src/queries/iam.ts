import {
  GetInstanceProfileCommand,
  GetPolicyCommand,
  GetPolicyVersionCommand,
  GetRoleCommand,
  GetUserCommand,
  ListAccessKeysCommand,
  ListAttachedRolePoliciesCommand,
  ListAttachedUserPoliciesCommand,
  ListInstanceProfilesCommand,
  ListInstanceProfilesForRoleCommand,
  ListPoliciesCommand,
  ListRolesCommand,
  ListUsersCommand,
} from "@aws-sdk/client-iam"
import { queryOptions } from "@tanstack/react-query"

import { getIamClient } from "@/lib/awsClient"

export const iamUsersQueryOptions = queryOptions({
  queryKey: ["iam", "users"],
  queryFn: async () => {
    const command = new ListUsersCommand({})
    return await getIamClient().send(command)
  },
  staleTime: 300_000,
})

export const iamUserQueryOptions = (userName: string) =>
  queryOptions({
    queryKey: ["iam", "users", userName],
    queryFn: async () => {
      const command = new GetUserCommand({ UserName: userName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamAccessKeysQueryOptions = (userName: string) =>
  queryOptions({
    queryKey: ["iam", "access-keys", userName],
    queryFn: async () => {
      const command = new ListAccessKeysCommand({ UserName: userName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamPoliciesQueryOptions = queryOptions({
  queryKey: ["iam", "policies"],
  queryFn: async () => {
    const command = new ListPoliciesCommand({ Scope: "Local" })
    return await getIamClient().send(command)
  },
  staleTime: 300_000,
})

export const iamPolicyQueryOptions = (policyArn: string) =>
  queryOptions({
    queryKey: ["iam", "policies", policyArn],
    queryFn: async () => {
      const command = new GetPolicyCommand({ PolicyArn: policyArn })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamPolicyVersionQueryOptions = (
  policyArn: string,
  versionId: string,
) =>
  queryOptions({
    queryKey: ["iam", "policy-versions", policyArn, versionId],
    queryFn: async () => {
      const command = new GetPolicyVersionCommand({
        PolicyArn: policyArn,
        VersionId: versionId,
      })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamAttachedUserPoliciesQueryOptions = (userName: string) =>
  queryOptions({
    queryKey: ["iam", "attached-user-policies", userName],
    queryFn: async () => {
      const command = new ListAttachedUserPoliciesCommand({
        UserName: userName,
      })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamRolesQueryOptions = queryOptions({
  queryKey: ["iam", "roles"],
  queryFn: async () => {
    const command = new ListRolesCommand({})
    return await getIamClient().send(command)
  },
  staleTime: 300_000,
})

export const iamRoleQueryOptions = (roleName: string) =>
  queryOptions({
    queryKey: ["iam", "roles", roleName],
    queryFn: async () => {
      const command = new GetRoleCommand({ RoleName: roleName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamAttachedRolePoliciesQueryOptions = (roleName: string) =>
  queryOptions({
    queryKey: ["iam", "attached-role-policies", roleName],
    queryFn: async () => {
      const command = new ListAttachedRolePoliciesCommand({
        RoleName: roleName,
      })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamInstanceProfilesQueryOptions = queryOptions({
  queryKey: ["iam", "instance-profiles"],
  queryFn: async () => {
    const command = new ListInstanceProfilesCommand({})
    return await getIamClient().send(command)
  },
  staleTime: 300_000,
})

export const iamInstanceProfileQueryOptions = (instanceProfileName: string) =>
  queryOptions({
    queryKey: ["iam", "instance-profiles", instanceProfileName],
    queryFn: async () => {
      const command = new GetInstanceProfileCommand({
        InstanceProfileName: instanceProfileName,
      })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamInstanceProfilesForRoleQueryOptions = (roleName: string) =>
  queryOptions({
    queryKey: ["iam", "instance-profiles-for-role", roleName],
    queryFn: async () => {
      const command = new ListInstanceProfilesForRoleCommand({
        RoleName: roleName,
      })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })
