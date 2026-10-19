import type { QueryClient } from "@tanstack/react-query"
import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { iamUsersQueryOptions } from "@/queries/iam"

import {
  useAddRoleToInstanceProfile,
  useAddUserToGroup,
  useAttachGroupPolicy,
  useAttachRolePolicy,
  useAttachUserPolicy,
  useCreateAccessKey,
  useCreateGroup,
  useCreateInstanceProfile,
  useCreatePolicy,
  useCreateRole,
  useCreateUser,
  useDeleteAccessKey,
  useDeleteGroup,
  useDeleteGroupPolicy,
  useDeleteInstanceProfile,
  useDeletePolicy,
  useDeleteRole,
  useDeleteRolePolicy,
  useDeleteUser,
  useDeleteUserPolicy,
  useDetachGroupPolicy,
  useDetachRolePolicy,
  useDetachUserPolicy,
  usePutGroupPolicy,
  usePutRolePolicy,
  usePutUserPolicy,
  useRemoveRoleFromInstanceProfile,
  useRemoveUserFromGroup,
  useUpdateAccessKey,
} from "./iam"

describe("useCreateUser", () => {
  it("sends CreateUserCommand with userName", async () => {
    const { result } = renderMutationHook(() => useCreateUser())

    result.current.mutate({ userName: "admin" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      Path: undefined,
    })
  })

  it("includes Path when provided", async () => {
    const { result } = renderMutationHook(() => useCreateUser())

    result.current.mutate({ userName: "admin", path: "/engineering/" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      Path: "/engineering/",
    })
  })

  it("marks the cached user list stale on success", async () => {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(iamUsersQueryOptions.queryKey, {
      Users: [],
      $metadata: {},
    })
    const { result } = renderMutationHook(() => useCreateUser(), queryClient)

    result.current.mutate({ userName: "admin" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(
      queryClient.getQueryState(iamUsersQueryOptions.queryKey)?.isInvalidated,
    ).toBeTruthy()
  })
})

describe("useDeleteUser", () => {
  it("sends DeleteUserCommand with userName", async () => {
    const { result } = renderMutationHook(() => useDeleteUser())

    result.current.mutate("admin")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })
})

describe("useCreateAccessKey", () => {
  it("sends CreateAccessKeyCommand with userName", async () => {
    const { result } = renderMutationHook(() => useCreateAccessKey())

    result.current.mutate("admin")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("invalidates access-keys query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreateAccessKey(),
      queryClient,
    )

    result.current.mutate("admin")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "access-keys"] })
  })
})

describe("useDeleteAccessKey", () => {
  it("sends DeleteAccessKeyCommand with userName and accessKeyId", async () => {
    const { result } = renderMutationHook(() => useDeleteAccessKey())

    result.current.mutate({ userName: "admin", accessKeyId: "AKIA123" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      AccessKeyId: "AKIA123",
    })
  })
})

describe("useUpdateAccessKey", () => {
  it("sends UpdateAccessKeyCommand with status", async () => {
    const { result } = renderMutationHook(() => useUpdateAccessKey())

    result.current.mutate({
      userName: "admin",
      accessKeyId: "AKIA123",
      status: "Inactive",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      AccessKeyId: "AKIA123",
      Status: "Inactive",
    })
  })
})

describe("useCreatePolicy", () => {
  it("sends CreatePolicyCommand with policy data", async () => {
    const { result } = renderMutationHook(() => useCreatePolicy())

    result.current.mutate({
      policyName: "ReadOnly",
      policyDocument: '{"Version":"2012-10-17"}',
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyName: "ReadOnly",
      Description: undefined,
      PolicyDocument: '{"Version":"2012-10-17"}',
    })
  })

  it("includes Description when provided", async () => {
    const { result } = renderMutationHook(() => useCreatePolicy())

    result.current.mutate({
      policyName: "ReadOnly",
      description: "Read-only access",
      policyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input.Description).toBe(
      "Read-only access",
    )
  })

  it("invalidates policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderMutationHook(() => useCreatePolicy(), queryClient)

    result.current.mutate({ policyName: "ReadOnly", policyDocument: "{}" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "policies"] })
  })
})

describe("useDeletePolicy", () => {
  it("sends DeletePolicyCommand with policyArn", async () => {
    const { result } = renderMutationHook(() => useDeletePolicy())

    result.current.mutate("arn:aws:iam::123:policy/ReadOnly")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyArn: "arn:aws:iam::123:policy/ReadOnly",
    })
  })
})

describe("useAttachUserPolicy", () => {
  it("sends AttachUserPolicyCommand with userName and policyArn", async () => {
    const { result } = renderMutationHook(() => useAttachUserPolicy())

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-user-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useAttachUserPolicy(),
      queryClient,
    )

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-user-policies"],
    })
  })
})

describe("useDetachUserPolicy", () => {
  it("sends DetachUserPolicyCommand with userName and policyArn", async () => {
    const { result } = renderMutationHook(() => useDetachUserPolicy())

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-user-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useDetachUserPolicy(),
      queryClient,
    )

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-user-policies"],
    })
  })
})

describe("usePutUserPolicy", () => {
  it("sends PutUserPolicyCommand with mapped fields", async () => {
    const { result } = renderMutationHook(() => usePutUserPolicy())

    result.current.mutate({
      name: "admin",
      policyName: "s3-read",
      policyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyName: "s3-read",
      PolicyDocument: "{}",
    })
  })

  it("invalidates the user inline-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderMutationHook(() => usePutUserPolicy(), queryClient)

    result.current.mutate({
      name: "admin",
      policyName: "s3-read",
      policyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "user-inline-policies", "admin"],
    })
  })
})

describe("useDeleteUserPolicy", () => {
  it("sends DeleteUserPolicyCommand with mapped fields", async () => {
    const { result } = renderMutationHook(() => useDeleteUserPolicy())

    result.current.mutate({ name: "admin", policyName: "s3-read" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyName: "s3-read",
    })
  })

  it("invalidates the user inline-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useDeleteUserPolicy(),
      queryClient,
    )

    result.current.mutate({ name: "admin", policyName: "s3-read" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "user-inline-policies", "admin"],
    })
  })
})

describe("usePutRolePolicy", () => {
  it("sends PutRolePolicyCommand with mapped fields", async () => {
    const { result } = renderMutationHook(() => usePutRolePolicy())

    result.current.mutate({
      name: "my-role",
      policyName: "s3-read",
      policyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyName: "s3-read",
      PolicyDocument: "{}",
    })
  })

  it("invalidates the role inline-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderMutationHook(() => usePutRolePolicy(), queryClient)

    result.current.mutate({
      name: "my-role",
      policyName: "s3-read",
      policyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "role-inline-policies", "my-role"],
    })
  })
})

describe("useDeleteRolePolicy", () => {
  it("sends DeleteRolePolicyCommand with mapped fields", async () => {
    const { result } = renderMutationHook(() => useDeleteRolePolicy())

    result.current.mutate({ name: "my-role", policyName: "s3-read" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyName: "s3-read",
    })
  })
})

describe("usePutGroupPolicy", () => {
  it("sends PutGroupPolicyCommand with mapped fields", async () => {
    const { result } = renderMutationHook(() => usePutGroupPolicy())

    result.current.mutate({
      name: "my-group",
      policyName: "s3-read",
      policyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyName: "s3-read",
      PolicyDocument: "{}",
    })
  })

  it("invalidates the group inline-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => usePutGroupPolicy(),
      queryClient,
    )

    result.current.mutate({
      name: "my-group",
      policyName: "s3-read",
      policyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "group-inline-policies", "my-group"],
    })
  })
})

describe("useDeleteGroupPolicy", () => {
  it("sends DeleteGroupPolicyCommand with mapped fields", async () => {
    const { result } = renderMutationHook(() => useDeleteGroupPolicy())

    result.current.mutate({ name: "my-group", policyName: "s3-read" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyName: "s3-read",
    })
  })
})

describe("useCreateRole", () => {
  it("sends CreateRoleCommand with role data", async () => {
    const { result } = renderMutationHook(() => useCreateRole())

    result.current.mutate({
      roleName: "my-role",
      assumeRolePolicyDocument: '{"Version":"2012-10-17"}',
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      Path: undefined,
      Description: undefined,
      AssumeRolePolicyDocument: '{"Version":"2012-10-17"}',
    })
  })

  it("includes Path and Description when provided", async () => {
    const { result } = renderMutationHook(() => useCreateRole())

    result.current.mutate({
      roleName: "my-role",
      path: "/service/",
      description: "EC2 role",
      assumeRolePolicyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      Path: "/service/",
      Description: "EC2 role",
      AssumeRolePolicyDocument: "{}",
    })
  })

  it("invalidates roles query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderMutationHook(() => useCreateRole(), queryClient)

    result.current.mutate({
      roleName: "my-role",
      assumeRolePolicyDocument: "{}",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "roles"] })
  })
})

describe("useDeleteRole", () => {
  it("sends DeleteRoleCommand with roleName", async () => {
    const { result } = renderMutationHook(() => useDeleteRole())

    result.current.mutate("my-role")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })
})

describe("useAttachRolePolicy", () => {
  it("sends AttachRolePolicyCommand with roleName and policyArn", async () => {
    const { result } = renderMutationHook(() => useAttachRolePolicy())

    result.current.mutate({ roleName: "my-role", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-role-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useAttachRolePolicy(),
      queryClient,
    )

    result.current.mutate({ roleName: "my-role", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-role-policies"],
    })
  })
})

describe("useDetachRolePolicy", () => {
  it("sends DetachRolePolicyCommand with roleName and policyArn", async () => {
    const { result } = renderMutationHook(() => useDetachRolePolicy())

    result.current.mutate({ roleName: "my-role", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyArn: "arn:test",
    })
  })
})

describe("useCreateInstanceProfile", () => {
  it("sends CreateInstanceProfileCommand with name", async () => {
    const { result } = renderMutationHook(() => useCreateInstanceProfile())

    result.current.mutate({ instanceProfileName: "my-profile" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
      Path: undefined,
    })
  })

  it("invalidates instance-profiles query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useCreateInstanceProfile(),
      queryClient,
    )

    result.current.mutate({ instanceProfileName: "my-profile" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "instance-profiles"],
    })
  })
})

describe("useDeleteInstanceProfile", () => {
  it("sends DeleteInstanceProfileCommand with name", async () => {
    const { result } = renderMutationHook(() => useDeleteInstanceProfile())

    result.current.mutate("my-profile")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
    })
  })
})

describe("useAddRoleToInstanceProfile", () => {
  it("sends AddRoleToInstanceProfileCommand with name and role", async () => {
    const { result } = renderMutationHook(() => useAddRoleToInstanceProfile())

    result.current.mutate({
      instanceProfileName: "my-profile",
      roleName: "my-role",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
      RoleName: "my-role",
    })
  })

  it("invalidates instance-profiles query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useAddRoleToInstanceProfile(),
      queryClient,
    )

    result.current.mutate({
      instanceProfileName: "my-profile",
      roleName: "my-role",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "instance-profiles"],
    })
  })
})

describe("useRemoveRoleFromInstanceProfile", () => {
  it("sends RemoveRoleFromInstanceProfileCommand with name and role", async () => {
    const { result } = renderMutationHook(() =>
      useRemoveRoleFromInstanceProfile(),
    )

    result.current.mutate({
      instanceProfileName: "my-profile",
      roleName: "my-role",
    })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
      RoleName: "my-role",
    })
  })
})

describe("instance profile membership changes", () => {
  const roleProfilesKey = ["iam", "instance-profiles-for-role", "app"]

  function seedRoleProfiles() {
    const queryClient = createTestQueryClient()
    queryClient.setQueryData(roleProfilesKey, { InstanceProfiles: [] })

    return queryClient
  }

  function roleProfilesInvalidated(queryClient: QueryClient) {
    return queryClient.getQueryCache().find({ queryKey: roleProfilesKey })
      ?.state.isInvalidated
  }

  it("refreshes the role page after adding a role", async () => {
    const queryClient = seedRoleProfiles()

    const { result } = renderMutationHook(
      () => useAddRoleToInstanceProfile(),
      queryClient,
    )

    result.current.mutate({ instanceProfileName: "p", roleName: "app" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(roleProfilesInvalidated(queryClient)).toBeTruthy()
  })

  it("refreshes the role page after removing a role", async () => {
    const queryClient = seedRoleProfiles()

    const { result } = renderMutationHook(
      () => useRemoveRoleFromInstanceProfile(),
      queryClient,
    )

    result.current.mutate({ instanceProfileName: "p", roleName: "app" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(roleProfilesInvalidated(queryClient)).toBeTruthy()
  })

  it("refreshes the role page after deleting a profile", async () => {
    const queryClient = seedRoleProfiles()

    const { result } = renderMutationHook(
      () => useDeleteInstanceProfile(),
      queryClient,
    )

    result.current.mutate("p")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(roleProfilesInvalidated(queryClient)).toBeTruthy()
  })
})

describe("useCreateGroup", () => {
  it("sends CreateGroupCommand with groupName", async () => {
    const { result } = renderMutationHook(() => useCreateGroup())

    result.current.mutate({ groupName: "my-group" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      Path: undefined,
    })
  })

  it("includes Path when provided", async () => {
    const { result } = renderMutationHook(() => useCreateGroup())

    result.current.mutate({ groupName: "my-group", path: "/engineering/" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      Path: "/engineering/",
    })
  })

  it("invalidates groups query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderMutationHook(() => useCreateGroup(), queryClient)

    result.current.mutate({ groupName: "my-group" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups"] })
  })
})

describe("useDeleteGroup", () => {
  it("sends DeleteGroupCommand with groupName", async () => {
    const { result } = renderMutationHook(() => useDeleteGroup())

    result.current.mutate("my-group")

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })
})

describe("useAttachGroupPolicy", () => {
  it("sends AttachGroupPolicyCommand with groupName and policyArn", async () => {
    const { result } = renderMutationHook(() => useAttachGroupPolicy())

    result.current.mutate({ groupName: "my-group", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-group-policies query on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useAttachGroupPolicy(),
      queryClient,
    )

    result.current.mutate({ groupName: "my-group", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-group-policies"],
    })
  })
})

describe("useDetachGroupPolicy", () => {
  it("sends DetachGroupPolicyCommand with groupName and policyArn", async () => {
    const { result } = renderMutationHook(() => useDetachGroupPolicy())

    result.current.mutate({ groupName: "my-group", policyArn: "arn:test" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyArn: "arn:test",
    })
  })
})

describe("useAddUserToGroup", () => {
  it("sends AddUserToGroupCommand with groupName and userName", async () => {
    const { result } = renderMutationHook(() => useAddUserToGroup())

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      UserName: "admin",
    })
  })

  it("invalidates groups and groups-for-user queries on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useAddUserToGroup(),
      queryClient,
    )

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups"] })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups-for-user"] })
  })
})

describe("useRemoveUserFromGroup", () => {
  it("sends RemoveUserFromGroupCommand with groupName and userName", async () => {
    const { result } = renderMutationHook(() => useRemoveUserFromGroup())

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      UserName: "admin",
    })
  })

  it("invalidates groups and groups-for-user queries on success", async () => {
    const queryClient = createTestQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")

    const { result } = renderMutationHook(
      () => useRemoveUserFromGroup(),
      queryClient,
    )

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => {
      expect(result.current.isSuccess).toBeTruthy()
    })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups"] })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups-for-user"] })
  })
})
