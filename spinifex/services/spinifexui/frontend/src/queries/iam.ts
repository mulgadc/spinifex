import {
  GetGroupCommand,
  GetGroupPolicyCommand,
  GetInstanceProfileCommand,
  GetPolicyCommand,
  GetPolicyVersionCommand,
  GetRoleCommand,
  GetRolePolicyCommand,
  GetUserCommand,
  GetUserPolicyCommand,
  ListAccessKeysCommand,
  ListAttachedGroupPoliciesCommand,
  ListAttachedRolePoliciesCommand,
  ListAttachedUserPoliciesCommand,
  ListGroupPoliciesCommand,
  ListGroupsCommand,
  ListGroupsForUserCommand,
  ListInstanceProfilesCommand,
  ListInstanceProfilesForRoleCommand,
  ListPoliciesCommand,
  ListRolePoliciesCommand,
  ListRolesCommand,
  ListUserPoliciesCommand,
  ListUsersCommand,
} from "@aws-sdk/client-iam"
import { queryOptions } from "@tanstack/react-query"

import { getIamClient } from "@/lib/awsClient"
import { decodePolicyDocument } from "@/lib/json"

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

export const iamUserPoliciesQueryOptions = (userName: string) =>
  queryOptions({
    queryKey: ["iam", "user-inline-policies", userName],
    queryFn: async () => {
      const command = new ListUserPoliciesCommand({ UserName: userName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamUserPolicyQueryOptions = (
  userName: string,
  policyName: string,
) =>
  queryOptions({
    queryKey: ["iam", "user-inline-policies", userName, policyName],
    queryFn: async () => {
      const command = new GetUserPolicyCommand({
        UserName: userName,
        PolicyName: policyName,
      })
      const result = await getIamClient().send(command)
      return result.PolicyDocument
        ? decodePolicyDocument(result.PolicyDocument, true)
        : ""
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

export const iamRolePoliciesQueryOptions = (roleName: string) =>
  queryOptions({
    queryKey: ["iam", "role-inline-policies", roleName],
    queryFn: async () => {
      const command = new ListRolePoliciesCommand({ RoleName: roleName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamRolePolicyQueryOptions = (
  roleName: string,
  policyName: string,
) =>
  queryOptions({
    queryKey: ["iam", "role-inline-policies", roleName, policyName],
    queryFn: async () => {
      const command = new GetRolePolicyCommand({
        RoleName: roleName,
        PolicyName: policyName,
      })
      const result = await getIamClient().send(command)
      return result.PolicyDocument
        ? decodePolicyDocument(result.PolicyDocument, false)
        : ""
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

export const iamGroupsQueryOptions = queryOptions({
  queryKey: ["iam", "groups"],
  queryFn: async () => {
    const command = new ListGroupsCommand({})
    return await getIamClient().send(command)
  },
  staleTime: 300_000,
})

export const iamGroupQueryOptions = (groupName: string) =>
  queryOptions({
    queryKey: ["iam", "groups", groupName],
    queryFn: async () => {
      const command = new GetGroupCommand({ GroupName: groupName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamAttachedGroupPoliciesQueryOptions = (groupName: string) =>
  queryOptions({
    queryKey: ["iam", "attached-group-policies", groupName],
    queryFn: async () => {
      const command = new ListAttachedGroupPoliciesCommand({
        GroupName: groupName,
      })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamGroupPoliciesQueryOptions = (groupName: string) =>
  queryOptions({
    queryKey: ["iam", "group-inline-policies", groupName],
    queryFn: async () => {
      const command = new ListGroupPoliciesCommand({ GroupName: groupName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })

export const iamGroupPolicyQueryOptions = (
  groupName: string,
  policyName: string,
) =>
  queryOptions({
    queryKey: ["iam", "group-inline-policies", groupName, policyName],
    queryFn: async () => {
      const command = new GetGroupPolicyCommand({
        GroupName: groupName,
        PolicyName: policyName,
      })
      const result = await getIamClient().send(command)
      return result.PolicyDocument
        ? decodePolicyDocument(result.PolicyDocument, true)
        : ""
    },
    staleTime: 300_000,
  })

export const iamGroupsForUserQueryOptions = (userName: string) =>
  queryOptions({
    queryKey: ["iam", "groups-for-user", userName],
    queryFn: async () => {
      const command = new ListGroupsForUserCommand({ UserName: userName })
      return await getIamClient().send(command)
    },
    staleTime: 300_000,
  })
