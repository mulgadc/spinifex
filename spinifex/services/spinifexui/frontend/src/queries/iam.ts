import {
  GetPolicyCommand,
  GetPolicyVersionCommand,
  GetUserCommand,
  ListAccessKeysCommand,
  ListAttachedUserPoliciesCommand,
  ListPoliciesCommand,
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
