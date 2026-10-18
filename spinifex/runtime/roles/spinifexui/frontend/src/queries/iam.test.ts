import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getIamClient: () => ({ send: mockSend }),
}))

import { callQueryFn } from "@/test/query"

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

describe("queryFn", () => {
  it("iamUsersQueryOptions sends ListUsersCommand", async () => {
    await callQueryFn(iamUsersQueryOptions)
    expect(mockSend).toHaveBeenCalledOnce()
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ MaxItems: 1000 })
  })

  it("iamUserQueryOptions sends GetUserCommand with userName", async () => {
    await callQueryFn(iamUserQueryOptions("admin"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamAccessKeysQueryOptions sends ListAccessKeysCommand", async () => {
    await callQueryFn(iamAccessKeysQueryOptions("admin"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamPoliciesQueryOptions sends ListPoliciesCommand with Local scope", async () => {
    await callQueryFn(iamPoliciesQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      Scope: "Local",
      MaxItems: 1000,
    })
  })

  it("iamPolicyQueryOptions sends GetPolicyCommand with policyArn", async () => {
    await callQueryFn(iamPolicyQueryOptions("arn:test"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyArn: "arn:test",
    })
  })

  it("iamPolicyVersionQueryOptions sends GetPolicyVersionCommand", async () => {
    await callQueryFn(iamPolicyVersionQueryOptions("arn:test", "v1"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyArn: "arn:test",
      VersionId: "v1",
    })
  })

  it("iamAttachedUserPoliciesQueryOptions sends ListAttachedUserPoliciesCommand", async () => {
    await callQueryFn(iamAttachedUserPoliciesQueryOptions("admin"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamRolesQueryOptions sends ListRolesCommand", async () => {
    await callQueryFn(iamRolesQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ MaxItems: 1000 })
  })

  it("iamRoleQueryOptions sends GetRoleCommand with roleName", async () => {
    await callQueryFn(iamRoleQueryOptions("my-role"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamAttachedRolePoliciesQueryOptions sends ListAttachedRolePoliciesCommand", async () => {
    await callQueryFn(iamAttachedRolePoliciesQueryOptions("my-role"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamInstanceProfilesQueryOptions sends ListInstanceProfilesCommand", async () => {
    await callQueryFn(iamInstanceProfilesQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ MaxItems: 1000 })
  })

  it("iamInstanceProfileQueryOptions sends GetInstanceProfileCommand", async () => {
    await callQueryFn(iamInstanceProfileQueryOptions("my-profile"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
    })
  })

  it("iamInstanceProfilesForRoleQueryOptions sends ListInstanceProfilesForRoleCommand", async () => {
    await callQueryFn(iamInstanceProfilesForRoleQueryOptions("my-role"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamGroupsQueryOptions sends ListGroupsCommand", async () => {
    await callQueryFn(iamGroupsQueryOptions)
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ MaxItems: 1000 })
  })

  it("iamGroupQueryOptions sends GetGroupCommand with groupName", async () => {
    await callQueryFn(iamGroupQueryOptions("my-group"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      MaxItems: 1000,
    })
  })

  it("iamAttachedGroupPoliciesQueryOptions sends ListAttachedGroupPoliciesCommand", async () => {
    await callQueryFn(iamAttachedGroupPoliciesQueryOptions("my-group"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("iamGroupsForUserQueryOptions sends ListGroupsForUserCommand with userName", async () => {
    await callQueryFn(iamGroupsForUserQueryOptions("admin"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamUserPoliciesQueryOptions sends ListUserPoliciesCommand", async () => {
    await callQueryFn(iamUserPoliciesQueryOptions("admin"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("iamUserPolicyQueryOptions sends GetUserPolicyCommand and decodes document", async () => {
    mockSend.mockResolvedValueOnce({
      PolicyDocument: encodeURIComponent('{"Version":"2012-10-17"}'),
    })

    const document = await callQueryFn(
      iamUserPolicyQueryOptions("admin", "s3-read"),
    )

    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyName: "s3-read",
    })
    expect(document).toBe('{\n  "Version": "2012-10-17"\n}')
  })

  it("iamRolePoliciesQueryOptions sends ListRolePoliciesCommand", async () => {
    await callQueryFn(iamRolePoliciesQueryOptions("my-role"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })

  it("iamRolePolicyQueryOptions sends GetRolePolicyCommand and decodes document", async () => {
    mockSend.mockResolvedValueOnce({
      PolicyDocument: encodeURIComponent('{"Version":"2012-10-17"}'),
    })

    const document = await callQueryFn(
      iamRolePolicyQueryOptions("my-role", "s3-read"),
    )

    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyName: "s3-read",
    })
    expect(document).toBe('{\n  "Version": "2012-10-17"\n}')
  })

  it("iamGroupPoliciesQueryOptions sends ListGroupPoliciesCommand", async () => {
    await callQueryFn(iamGroupPoliciesQueryOptions("my-group"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })

  it("iamGroupPolicyQueryOptions sends GetGroupPolicyCommand with names", async () => {
    await callQueryFn(iamGroupPolicyQueryOptions("my-group", "s3-read"))
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyName: "s3-read",
    })
  })
})
