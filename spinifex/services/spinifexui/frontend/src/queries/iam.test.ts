import { afterEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getIamClient: () => ({ send: mockSend }),
}))

import {
  iamAccessKeysQueryOptions,
  iamAttachedGroupPoliciesQueryOptions,
  iamAttachedRolePoliciesQueryOptions,
  iamAttachedUserPoliciesQueryOptions,
  iamGroupPoliciesQueryOptions,
  iamGroupPolicyQueryOptions,
  iamGroupQueryOptions,
  iamGroupsForUserQueryOptions,
  iamGroupsQueryOptions,
  iamInstanceProfileQueryOptions,
  iamInstanceProfilesForRoleQueryOptions,
  iamInstanceProfilesQueryOptions,
  iamPoliciesQueryOptions,
  iamPolicyQueryOptions,
  iamPolicyVersionQueryOptions,
  iamRolePoliciesQueryOptions,
  iamRolePolicyQueryOptions,
  iamRoleQueryOptions,
  iamRolesQueryOptions,
  iamUserPoliciesQueryOptions,
  iamUserPolicyQueryOptions,
  iamUserQueryOptions,
  iamUsersQueryOptions,
} from "./iam"

describe("query keys", () => {
  it("iamUsersQueryOptions has correct key", () => {
    expect(iamUsersQueryOptions.queryKey).toStrictEqual(["iam", "users"])
  })

  it("iamUserQueryOptions includes userName in key", () => {
    expect(iamUserQueryOptions("admin").queryKey).toStrictEqual([
      "iam",
      "users",
      "admin",
    ])
  })

  it("iamAccessKeysQueryOptions includes userName in key", () => {
    expect(iamAccessKeysQueryOptions("admin").queryKey).toStrictEqual([
      "iam",
      "access-keys",
      "admin",
    ])
  })

  it("iamPoliciesQueryOptions has correct key", () => {
    expect(iamPoliciesQueryOptions.queryKey).toStrictEqual(["iam", "policies"])
  })

  it("iamPolicyQueryOptions includes policyArn in key", () => {
    expect(
      iamPolicyQueryOptions("arn:aws:iam::123:policy/ReadOnly").queryKey,
    ).toStrictEqual(["iam", "policies", "arn:aws:iam::123:policy/ReadOnly"])
  })

  it("iamPolicyVersionQueryOptions includes policyArn and versionId in key", () => {
    expect(
      iamPolicyVersionQueryOptions("arn:aws:iam::123:policy/ReadOnly", "v1")
        .queryKey,
    ).toStrictEqual([
      "iam",
      "policy-versions",
      "arn:aws:iam::123:policy/ReadOnly",
      "v1",
    ])
  })

  it("iamAttachedUserPoliciesQueryOptions includes userName in key", () => {
    expect(iamAttachedUserPoliciesQueryOptions("admin").queryKey).toStrictEqual(
      ["iam", "attached-user-policies", "admin"],
    )
  })

  it("iamRolesQueryOptions has correct key", () => {
    expect(iamRolesQueryOptions.queryKey).toStrictEqual(["iam", "roles"])
  })

  it("iamRoleQueryOptions includes roleName in key", () => {
    expect(iamRoleQueryOptions("my-role").queryKey).toStrictEqual([
      "iam",
      "roles",
      "my-role",
    ])
  })

  it("iamAttachedRolePoliciesQueryOptions includes roleName in key", () => {
    expect(
      iamAttachedRolePoliciesQueryOptions("my-role").queryKey,
    ).toStrictEqual(["iam", "attached-role-policies", "my-role"])
  })

  it("iamInstanceProfilesQueryOptions has correct key", () => {
    expect(iamInstanceProfilesQueryOptions.queryKey).toStrictEqual([
      "iam",
      "instance-profiles",
    ])
  })

  it("iamInstanceProfileQueryOptions includes name in key", () => {
    expect(iamInstanceProfileQueryOptions("my-profile").queryKey).toStrictEqual(
      ["iam", "instance-profiles", "my-profile"],
    )
  })

  it("iamInstanceProfilesForRoleQueryOptions includes roleName in key", () => {
    expect(
      iamInstanceProfilesForRoleQueryOptions("my-role").queryKey,
    ).toStrictEqual(["iam", "instance-profiles-for-role", "my-role"])
  })

  it("iamGroupsQueryOptions has correct key", () => {
    expect(iamGroupsQueryOptions.queryKey).toStrictEqual(["iam", "groups"])
  })

  it("iamGroupQueryOptions includes groupName in key", () => {
    expect(iamGroupQueryOptions("my-group").queryKey).toStrictEqual([
      "iam",
      "groups",
      "my-group",
    ])
  })

  it("iamAttachedGroupPoliciesQueryOptions includes groupName in key", () => {
    expect(
      iamAttachedGroupPoliciesQueryOptions("my-group").queryKey,
    ).toStrictEqual(["iam", "attached-group-policies", "my-group"])
  })

  it("iamGroupsForUserQueryOptions includes userName in key", () => {
    expect(iamGroupsForUserQueryOptions("admin").queryKey).toStrictEqual([
      "iam",
      "groups-for-user",
      "admin",
    ])
  })

  it("iamUserPoliciesQueryOptions includes userName in key", () => {
    expect(iamUserPoliciesQueryOptions("admin").queryKey).toStrictEqual([
      "iam",
      "user-inline-policies",
      "admin",
    ])
  })

  it("iamUserPolicyQueryOptions includes userName and policyName in key", () => {
    expect(
      iamUserPolicyQueryOptions("admin", "s3-read").queryKey,
    ).toStrictEqual(["iam", "user-inline-policies", "admin", "s3-read"])
  })

  it("iamRolePoliciesQueryOptions includes roleName in key", () => {
    expect(iamRolePoliciesQueryOptions("my-role").queryKey).toStrictEqual([
      "iam",
      "role-inline-policies",
      "my-role",
    ])
  })

  it("iamRolePolicyQueryOptions includes roleName and policyName in key", () => {
    expect(
      iamRolePolicyQueryOptions("my-role", "s3-read").queryKey,
    ).toStrictEqual(["iam", "role-inline-policies", "my-role", "s3-read"])
  })

  it("iamGroupPoliciesQueryOptions includes groupName in key", () => {
    expect(iamGroupPoliciesQueryOptions("my-group").queryKey).toStrictEqual([
      "iam",
      "group-inline-policies",
      "my-group",
    ])
  })

  it("iamGroupPolicyQueryOptions includes groupName and policyName in key", () => {
    expect(
      iamGroupPolicyQueryOptions("my-group", "s3-read").queryKey,
    ).toStrictEqual(["iam", "group-inline-policies", "my-group", "s3-read"])
  })
})

describe("staleTime", () => {
  it("users use staleTime", () => {
    expect(iamUsersQueryOptions.staleTime).toBe(300_000)
  })

  it("user uses staleTime", () => {
    expect(iamUserQueryOptions("admin").staleTime).toBe(300_000)
  })

  it("access keys use staleTime", () => {
    expect(iamAccessKeysQueryOptions("admin").staleTime).toBe(300_000)
  })

  it("policies use staleTime", () => {
    expect(iamPoliciesQueryOptions.staleTime).toBe(300_000)
  })

  it("policy uses staleTime", () => {
    expect(iamPolicyQueryOptions("arn:test").staleTime).toBe(300_000)
  })

  it("policy version uses staleTime", () => {
    expect(iamPolicyVersionQueryOptions("arn:test", "v1").staleTime).toBe(
      300_000,
    )
  })

  it("attached user policies use staleTime", () => {
    expect(iamAttachedUserPoliciesQueryOptions("admin").staleTime).toBe(300_000)
  })
})

describe("queryFn", () => {
  afterEach(() => {
    mockSend.mockClear()
  })

  it("iamUsersQueryOptions sends ListUsersCommand", async () => {
    const queryFn = iamUsersQueryOptions.queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("iamUserQueryOptions sends GetUserCommand with userName", async () => {
    const queryFn = iamUserQueryOptions("admin").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamAccessKeysQueryOptions sends ListAccessKeysCommand", async () => {
    const queryFn = iamAccessKeysQueryOptions("admin").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamPoliciesQueryOptions sends ListPoliciesCommand with Local scope", async () => {
    const queryFn = iamPoliciesQueryOptions.queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ Scope: "Local" })
  })

  it("iamPolicyQueryOptions sends GetPolicyCommand with policyArn", async () => {
    const queryFn = iamPolicyQueryOptions("arn:test").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyArn: "arn:test",
    })
  })

  it("iamPolicyVersionQueryOptions sends GetPolicyVersionCommand", async () => {
    const queryFn = iamPolicyVersionQueryOptions("arn:test", "v1").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyArn: "arn:test",
      VersionId: "v1",
    })
  })

  it("iamAttachedUserPoliciesQueryOptions sends ListAttachedUserPoliciesCommand", async () => {
    const queryFn = iamAttachedUserPoliciesQueryOptions("admin").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamRolesQueryOptions sends ListRolesCommand", async () => {
    const queryFn = iamRolesQueryOptions.queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("iamRoleQueryOptions sends GetRoleCommand with roleName", async () => {
    const queryFn = iamRoleQueryOptions("my-role").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamAttachedRolePoliciesQueryOptions sends ListAttachedRolePoliciesCommand", async () => {
    const queryFn = iamAttachedRolePoliciesQueryOptions("my-role").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamInstanceProfilesQueryOptions sends ListInstanceProfilesCommand", async () => {
    const queryFn = iamInstanceProfilesQueryOptions.queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("iamInstanceProfileQueryOptions sends GetInstanceProfileCommand", async () => {
    const queryFn = iamInstanceProfileQueryOptions("my-profile").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
    })
  })

  it("iamInstanceProfilesForRoleQueryOptions sends ListInstanceProfilesForRoleCommand", async () => {
    const queryFn = iamInstanceProfilesForRoleQueryOptions("my-role")
      .queryFn as (ctx: never) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamGroupsQueryOptions sends ListGroupsCommand", async () => {
    const queryFn = iamGroupsQueryOptions.queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({})
  })

  it("iamGroupQueryOptions sends GetGroupCommand with groupName", async () => {
    const queryFn = iamGroupQueryOptions("my-group").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("iamAttachedGroupPoliciesQueryOptions sends ListAttachedGroupPoliciesCommand", async () => {
    const queryFn = iamAttachedGroupPoliciesQueryOptions("my-group")
      .queryFn as (ctx: never) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("iamGroupsForUserQueryOptions sends ListGroupsForUserCommand with userName", async () => {
    const queryFn = iamGroupsForUserQueryOptions("admin").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamUserPoliciesQueryOptions sends ListUserPoliciesCommand", async () => {
    const queryFn = iamUserPoliciesQueryOptions("admin").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamUserPolicyQueryOptions sends GetUserPolicyCommand and decodes document", async () => {
    mockSend.mockResolvedValueOnce({
      PolicyDocument: encodeURIComponent('{"Version":"2012-10-17"}'),
    })
    const queryFn = iamUserPolicyQueryOptions("admin", "s3-read").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    const document = await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyName: "s3-read",
    })
    expect(document).toBe('{\n  "Version": "2012-10-17"\n}')
  })

  it("iamRolePoliciesQueryOptions sends ListRolePoliciesCommand", async () => {
    const queryFn = iamRolePoliciesQueryOptions("my-role").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamRolePolicyQueryOptions sends GetRolePolicyCommand and formats raw JSON", async () => {
    mockSend.mockResolvedValueOnce({
      PolicyDocument: '{"Version":"2012-10-17"}',
    })
    const queryFn = iamRolePolicyQueryOptions("my-role", "s3-read").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    const document = await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyName: "s3-read",
    })
    expect(document).toBe('{\n  "Version": "2012-10-17"\n}')
  })

  it("iamGroupPoliciesQueryOptions sends ListGroupPoliciesCommand", async () => {
    const queryFn = iamGroupPoliciesQueryOptions("my-group").queryFn as (
      ctx: never,
    ) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("iamGroupPolicyQueryOptions sends GetGroupPolicyCommand with names", async () => {
    const queryFn = iamGroupPolicyQueryOptions("my-group", "s3-read")
      .queryFn as (ctx: never) => Promise<unknown>
    await queryFn({} as never)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyName: "s3-read",
    })
  })
})
