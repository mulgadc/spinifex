import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { renderHook, waitFor } from "@testing-library/react"
import type { ReactNode } from "react"
import { beforeEach, describe, expect, it, vi } from "vitest"

const mockSend = vi.fn().mockResolvedValue({})

vi.mock("@/lib/awsClient", () => ({
  getEksClient: () => ({ send: mockSend }),
}))

import {
  useAssociateAccessPolicy,
  useCreateAccessEntry,
  useCreateAddon,
  useCreateCluster,
  useCreateNodegroup,
  useDeleteAddon,
  useDeleteCluster,
  useDeleteNodegroup,
  useScaleNodegroup,
  useUpdateAddon,
  useUpdateNodegroupVersion,
} from "./eks"

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

describe("eks mutations", () => {
  beforeEach(() => {
    mockSend.mockClear()
  })

  describe("useCreateCluster", () => {
    it("maps form data to CreateClusterCommand with API auth mode", async () => {
      createQueryClient()
      const { result } = renderHook(() => useCreateCluster(), { wrapper })

      result.current.mutate({
        name: "c1",
        version: "1.32",
        roleArn: "arn:role",
        vpcId: "vpc-1",
        subnetIds: ["subnet-1"],
        securityGroupIds: ["sg-1"],
        bootstrapClusterCreatorAdminPermissions: true,
        endpointPublicAccess: true,
        endpointPrivateAccess: false,
        publicAccessCidrs: ["203.0.113.0/24"],
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        name: "c1",
        version: "1.32",
        roleArn: "arn:role",
        resourcesVpcConfig: {
          subnetIds: ["subnet-1"],
          securityGroupIds: ["sg-1"],
          endpointPublicAccess: true,
          endpointPrivateAccess: false,
          publicAccessCidrs: ["203.0.113.0/24"],
        },
        accessConfig: {
          authenticationMode: "API",
          bootstrapClusterCreatorAdminPermissions: true,
        },
      })
    })

    it("omits public access CIDRs for a private-only cluster", async () => {
      createQueryClient()
      const { result } = renderHook(() => useCreateCluster(), { wrapper })

      result.current.mutate({
        name: "c1",
        version: "1.32",
        roleArn: "arn:role",
        vpcId: "vpc-1",
        subnetIds: ["subnet-1"],
        securityGroupIds: ["sg-1"],
        bootstrapClusterCreatorAdminPermissions: true,
        endpointPublicAccess: false,
        endpointPrivateAccess: true,
        publicAccessCidrs: ["203.0.113.0/24"],
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(
        mockSend.mock.calls[0]?.[0].input.resourcesVpcConfig,
      ).toStrictEqual({
        subnetIds: ["subnet-1"],
        securityGroupIds: ["sg-1"],
        endpointPublicAccess: false,
        endpointPrivateAccess: true,
        publicAccessCidrs: undefined,
      })
    })
  })

  describe("useDeleteCluster", () => {
    it("sends DeleteClusterCommand with name", async () => {
      createQueryClient()
      const { result } = renderHook(() => useDeleteCluster(), { wrapper })

      result.current.mutate("c1")

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ name: "c1" })
    })
  })

  describe("useCreateNodegroup", () => {
    it("maps form data to CreateNodegroupCommand", async () => {
      createQueryClient()
      const { result } = renderHook(() => useCreateNodegroup(), { wrapper })

      result.current.mutate({
        clusterName: "c1",
        nodegroupName: "ng1",
        nodeRole: "arn:noderole",
        subnetIds: ["subnet-1"],
        instanceTypes: ["t3.medium"],
        amiType: "AL2_x86_64",
        capacityType: "ON_DEMAND",
        diskSize: 20,
        minSize: 1,
        desiredSize: 2,
        maxSize: 3,
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        nodegroupName: "ng1",
        nodeRole: "arn:noderole",
        subnets: ["subnet-1"],
        instanceTypes: ["t3.medium"],
        amiType: "AL2_x86_64",
        capacityType: "ON_DEMAND",
        diskSize: 20,
        scalingConfig: { minSize: 1, maxSize: 3, desiredSize: 2 },
      })
    })
  })

  describe("useScaleNodegroup", () => {
    it("sends UpdateNodegroupConfigCommand with scaling config", async () => {
      createQueryClient()
      const { result } = renderHook(() => useScaleNodegroup(), { wrapper })

      result.current.mutate({
        clusterName: "c1",
        nodegroupName: "ng1",
        minSize: 2,
        maxSize: 5,
        desiredSize: 3,
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        nodegroupName: "ng1",
        scalingConfig: { minSize: 2, maxSize: 5, desiredSize: 3 },
      })
    })
  })

  describe("useUpdateNodegroupVersion", () => {
    it("sends UpdateNodegroupVersionCommand with target version", async () => {
      createQueryClient()
      const { result } = renderHook(() => useUpdateNodegroupVersion(), {
        wrapper,
      })

      result.current.mutate({
        clusterName: "c1",
        nodegroupName: "ng1",
        version: "1.32",
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        nodegroupName: "ng1",
        version: "1.32",
      })
    })
  })

  describe("useDeleteNodegroup", () => {
    it("sends DeleteNodegroupCommand", async () => {
      createQueryClient()
      const { result } = renderHook(() => useDeleteNodegroup(), { wrapper })

      result.current.mutate({ clusterName: "c1", nodegroupName: "ng1" })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        nodegroupName: "ng1",
      })
    })
  })

  describe("useCreateAddon", () => {
    it("maps params to CreateAddonCommand", async () => {
      createQueryClient()
      const { result } = renderHook(() => useCreateAddon(), { wrapper })

      result.current.mutate({
        clusterName: "c1",
        addonName: "coredns",
        addonVersion: "v1.0.0",
        serviceAccountRoleArn: "arn:role",
        configurationValues: '{"replicaCount":2}',
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        addonName: "coredns",
        addonVersion: "v1.0.0",
        serviceAccountRoleArn: "arn:role",
        configurationValues: '{"replicaCount":2}',
      })
    })
  })

  describe("useUpdateAddon", () => {
    it("sends UpdateAddonCommand with target version", async () => {
      createQueryClient()
      const { result } = renderHook(() => useUpdateAddon(), { wrapper })

      result.current.mutate({
        clusterName: "c1",
        addonName: "coredns",
        addonVersion: "v2.0.0",
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        addonName: "coredns",
        addonVersion: "v2.0.0",
        serviceAccountRoleArn: undefined,
        configurationValues: undefined,
      })
    })
  })

  describe("useDeleteAddon", () => {
    it("sends DeleteAddonCommand", async () => {
      createQueryClient()
      const { result } = renderHook(() => useDeleteAddon(), { wrapper })

      result.current.mutate({ clusterName: "c1", addonName: "coredns" })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        addonName: "coredns",
      })
    })
  })

  describe("useCreateAccessEntry", () => {
    it("sends CreateAccessEntryCommand with kubernetes groups", async () => {
      createQueryClient()
      const { result } = renderHook(() => useCreateAccessEntry(), { wrapper })

      result.current.mutate({
        clusterName: "c1",
        principalArn: "arn:p",
        kubernetesGroups: ["system:masters"],
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        principalArn: "arn:p",
        kubernetesGroups: ["system:masters"],
        type: undefined,
      })
    })
  })

  describe("useAssociateAccessPolicy", () => {
    it("sends AssociateAccessPolicyCommand with cluster scope", async () => {
      createQueryClient()
      const { result } = renderHook(() => useAssociateAccessPolicy(), {
        wrapper,
      })

      result.current.mutate({
        clusterName: "c1",
        principalArn: "arn:p",
        policyArn: "arn:policy",
        accessScopeType: "cluster",
      })

      await waitFor(() => expect(result.current.isSuccess).toBeTruthy())
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        principalArn: "arn:p",
        policyArn: "arn:policy",
        accessScope: { type: "cluster", namespaces: undefined },
      })
    })
  })
})
