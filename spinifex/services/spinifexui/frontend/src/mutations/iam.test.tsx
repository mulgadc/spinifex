import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getIamClient: () => ({ send: mockSend }),
}))

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
  useDeleteInstanceProfile,
  useDeletePolicy,
  useDeleteRole,
  useDeleteUser,
  useDetachGroupPolicy,
  useDetachRolePolicy,
  useDetachUserPolicy,
  useRemoveRoleFromInstanceProfile,
  useRemoveUserFromGroup,
  useUpdateAccessKey,
} from "./iam"

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

function createQueryClient() {
  queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  return queryClient
}

describe("useCreateUser", () => {
  it("sends CreateUserCommand with userName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateUser(), { wrapper })

    result.current.mutate({ userName: "admin" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      Path: undefined,
    })
  })

  it("includes Path when provided", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateUser(), { wrapper })

    result.current.mutate({ userName: "admin", path: "/engineering/" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      Path: "/engineering/",
    })
  })

  it("invalidates users query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateUser(), { wrapper })

    result.current.mutate({ userName: "admin" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "users"] })
  })
})

describe("useDeleteUser", () => {
  it("sends DeleteUserCommand with userName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteUser(), { wrapper })

    result.current.mutate("admin")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })
})

describe("useCreateAccessKey", () => {
  it("sends CreateAccessKeyCommand with userName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateAccessKey(), { wrapper })

    result.current.mutate("admin")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
    })
  })

  it("invalidates access-keys query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateAccessKey(), { wrapper })

    result.current.mutate("admin")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "access-keys"] })
  })
})

describe("useDeleteAccessKey", () => {
  it("sends DeleteAccessKeyCommand with userName and accessKeyId", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteAccessKey(), { wrapper })

    result.current.mutate({ userName: "admin", accessKeyId: "AKIA123" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      AccessKeyId: "AKIA123",
    })
  })
})

describe("useUpdateAccessKey", () => {
  it("sends UpdateAccessKeyCommand with status", async () => {
    createQueryClient()
    const { result } = renderHook(() => useUpdateAccessKey(), { wrapper })

    result.current.mutate({
      userName: "admin",
      accessKeyId: "AKIA123",
      status: "Inactive",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      AccessKeyId: "AKIA123",
      Status: "Inactive",
    })
  })
})

describe("useCreatePolicy", () => {
  it("sends CreatePolicyCommand with policy data", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreatePolicy(), { wrapper })

    result.current.mutate({
      policyName: "ReadOnly",
      policyDocument: '{"Version":"2012-10-17"}',
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyName: "ReadOnly",
      Description: undefined,
      PolicyDocument: '{"Version":"2012-10-17"}',
    })
  })

  it("includes Description when provided", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreatePolicy(), { wrapper })

    result.current.mutate({
      policyName: "ReadOnly",
      description: "Read-only access",
      policyDocument: "{}",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input.Description).toBe(
      "Read-only access",
    )
  })

  it("invalidates policies query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreatePolicy(), { wrapper })

    result.current.mutate({ policyName: "ReadOnly", policyDocument: "{}" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "policies"] })
  })
})

describe("useDeletePolicy", () => {
  it("sends DeletePolicyCommand with policyArn", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeletePolicy(), { wrapper })

    result.current.mutate("arn:aws:iam::123:policy/ReadOnly")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      PolicyArn: "arn:aws:iam::123:policy/ReadOnly",
    })
  })
})

describe("useAttachUserPolicy", () => {
  it("sends AttachUserPolicyCommand with userName and policyArn", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAttachUserPolicy(), { wrapper })

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-user-policies query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useAttachUserPolicy(), { wrapper })

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-user-policies"],
    })
  })
})

describe("useDetachUserPolicy", () => {
  it("sends DetachUserPolicyCommand with userName and policyArn", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDetachUserPolicy(), { wrapper })

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      UserName: "admin",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-user-policies query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useDetachUserPolicy(), { wrapper })

    result.current.mutate({ userName: "admin", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-user-policies"],
    })
  })
})

describe("useCreateRole", () => {
  it("sends CreateRoleCommand with role data", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateRole(), { wrapper })

    result.current.mutate({
      roleName: "my-role",
      assumeRolePolicyDocument: '{"Version":"2012-10-17"}',
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      Path: undefined,
      Description: undefined,
      AssumeRolePolicyDocument: '{"Version":"2012-10-17"}',
    })
  })

  it("includes Path and Description when provided", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateRole(), { wrapper })

    result.current.mutate({
      roleName: "my-role",
      path: "/service/",
      description: "EC2 role",
      assumeRolePolicyDocument: "{}",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      Path: "/service/",
      Description: "EC2 role",
      AssumeRolePolicyDocument: "{}",
    })
  })

  it("invalidates roles query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateRole(), { wrapper })

    result.current.mutate({
      roleName: "my-role",
      assumeRolePolicyDocument: "{}",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "roles"] })
  })
})

describe("useDeleteRole", () => {
  it("sends DeleteRoleCommand with roleName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteRole(), { wrapper })

    result.current.mutate("my-role")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
    })
  })
})

describe("useAttachRolePolicy", () => {
  it("sends AttachRolePolicyCommand with roleName and policyArn", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAttachRolePolicy(), { wrapper })

    result.current.mutate({ roleName: "my-role", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-role-policies query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useAttachRolePolicy(), { wrapper })

    result.current.mutate({ roleName: "my-role", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-role-policies"],
    })
  })
})

describe("useDetachRolePolicy", () => {
  it("sends DetachRolePolicyCommand with roleName and policyArn", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDetachRolePolicy(), { wrapper })

    result.current.mutate({ roleName: "my-role", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      RoleName: "my-role",
      PolicyArn: "arn:test",
    })
  })
})

describe("useCreateInstanceProfile", () => {
  it("sends CreateInstanceProfileCommand with name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateInstanceProfile(), { wrapper })

    result.current.mutate({ instanceProfileName: "my-profile" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
      Path: undefined,
    })
  })

  it("invalidates instance-profiles query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateInstanceProfile(), { wrapper })

    result.current.mutate({ instanceProfileName: "my-profile" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "instance-profiles"],
    })
  })
})

describe("useDeleteInstanceProfile", () => {
  it("sends DeleteInstanceProfileCommand with name", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteInstanceProfile(), { wrapper })

    result.current.mutate("my-profile")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
    })
  })
})

describe("useAddRoleToInstanceProfile", () => {
  it("sends AddRoleToInstanceProfileCommand with name and role", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAddRoleToInstanceProfile(), {
      wrapper,
    })

    result.current.mutate({
      instanceProfileName: "my-profile",
      roleName: "my-role",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
      RoleName: "my-role",
    })
  })

  it("invalidates instance-profiles query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useAddRoleToInstanceProfile(), {
      wrapper,
    })

    result.current.mutate({
      instanceProfileName: "my-profile",
      roleName: "my-role",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "instance-profiles"],
    })
  })
})

describe("useRemoveRoleFromInstanceProfile", () => {
  it("sends RemoveRoleFromInstanceProfileCommand with name and role", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRemoveRoleFromInstanceProfile(), {
      wrapper,
    })

    result.current.mutate({
      instanceProfileName: "my-profile",
      roleName: "my-role",
    })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      InstanceProfileName: "my-profile",
      RoleName: "my-role",
    })
  })
})

describe("useCreateGroup", () => {
  it("sends CreateGroupCommand with groupName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      Path: undefined,
    })
  })

  it("includes Path when provided", async () => {
    createQueryClient()
    const { result } = renderHook(() => useCreateGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group", path: "/engineering/" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      Path: "/engineering/",
    })
  })

  it("invalidates groups query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useCreateGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups"] })
  })
})

describe("useDeleteGroup", () => {
  it("sends DeleteGroupCommand with groupName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDeleteGroup(), { wrapper })

    result.current.mutate("my-group")

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
    })
  })
})

describe("useAttachGroupPolicy", () => {
  it("sends AttachGroupPolicyCommand with groupName and policyArn", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAttachGroupPolicy(), { wrapper })

    result.current.mutate({ groupName: "my-group", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyArn: "arn:test",
    })
  })

  it("invalidates attached-group-policies query on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useAttachGroupPolicy(), { wrapper })

    result.current.mutate({ groupName: "my-group", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({
      queryKey: ["iam", "attached-group-policies"],
    })
  })
})

describe("useDetachGroupPolicy", () => {
  it("sends DetachGroupPolicyCommand with groupName and policyArn", async () => {
    createQueryClient()
    const { result } = renderHook(() => useDetachGroupPolicy(), { wrapper })

    result.current.mutate({ groupName: "my-group", policyArn: "arn:test" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      PolicyArn: "arn:test",
    })
  })
})

describe("useAddUserToGroup", () => {
  it("sends AddUserToGroupCommand with groupName and userName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useAddUserToGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      UserName: "admin",
    })
  })

  it("invalidates groups and groups-for-user queries on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useAddUserToGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups"] })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups-for-user"] })
  })
})

describe("useRemoveUserFromGroup", () => {
  it("sends RemoveUserFromGroupCommand with groupName and userName", async () => {
    createQueryClient()
    const { result } = renderHook(() => useRemoveUserFromGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
      GroupName: "my-group",
      UserName: "admin",
    })
  })

  it("invalidates groups and groups-for-user queries on success", async () => {
    createQueryClient()
    const spy = vi.spyOn(queryClient, "invalidateQueries")
    const { result } = renderHook(() => useRemoveUserFromGroup(), { wrapper })

    result.current.mutate({ groupName: "my-group", userName: "admin" })

    await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups"] })
    expect(spy).toHaveBeenCalledWith({ queryKey: ["iam", "groups-for-user"] })
  })
})
