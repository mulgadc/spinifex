import { waitFor } from "@testing-library/react"
import { describe, expect, it, vi } from "vitest"

import { createTestQueryClient, renderMutationHook } from "@/test/render"

const mockSend = vi.hoisted(() => vi.fn().mockResolvedValue({}))

vi.mock("@/lib/awsClient", async () => {
  const { mockAwsClients } = await import("@/test/aws-clients")

  return mockAwsClients(mockSend)
})

import { eksNodegroupsQueryOptions } from "@/queries/eks"

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
} from "./eks"

describe("eks mutations", () => {
  describe("useCreateCluster", () => {
    it("maps form data to CreateClusterCommand with API auth mode", async () => {
      const { result } = renderMutationHook(() => useCreateCluster())

      result.current.mutate({
        name: "c1",
        version: "1.32",
        roleArn: "arn:role",
        vpcId: "vpc-1",
        subnetIds: ["subnet-1"],
        bootstrapClusterCreatorAdminPermissions: true,
        endpointPublicAccess: true,
        endpointPrivateAccess: false,
        publicAccessCidrs: ["203.0.113.0/24"],
      })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        name: "c1",
        version: "1.32",
        roleArn: "arn:role",
        resourcesVpcConfig: {
          subnetIds: ["subnet-1"],
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
      const { result } = renderMutationHook(() => useCreateCluster())

      result.current.mutate({
        name: "c1",
        version: "1.32",
        roleArn: "arn:role",
        vpcId: "vpc-1",
        subnetIds: ["subnet-1"],
        bootstrapClusterCreatorAdminPermissions: true,
        endpointPublicAccess: false,
        endpointPrivateAccess: true,
        publicAccessCidrs: ["203.0.113.0/24"],
      })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(
        mockSend.mock.calls[0]?.[0].input.resourcesVpcConfig,
      ).toStrictEqual({
        subnetIds: ["subnet-1"],
        endpointPublicAccess: false,
        endpointPrivateAccess: true,
        publicAccessCidrs: undefined,
      })
    })
  })

  describe("useDeleteCluster", () => {
    it("sends DeleteClusterCommand with name", async () => {
      const { result } = renderMutationHook(() => useDeleteCluster())

      result.current.mutate("c1")

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({ name: "c1" })
    })
  })

  describe("useCreateNodegroup", () => {
    it("maps form data to CreateNodegroupCommand", async () => {
      const { result } = renderMutationHook(() => useCreateNodegroup())

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

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
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

    it("marks the cluster's cached nodegroups stale on success", async () => {
      const queryClient = createTestQueryClient()
      queryClient.setQueryData(eksNodegroupsQueryOptions("c1").queryKey, {
        $metadata: {},
      })

      const { result } = renderMutationHook(
        () => useCreateNodegroup(),
        queryClient,
      )

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

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(
        queryClient.getQueryState(eksNodegroupsQueryOptions("c1").queryKey)
          ?.isInvalidated,
      ).toBeTruthy()
    })
  })

  describe("useScaleNodegroup", () => {
    it("sends UpdateNodegroupConfigCommand with scaling config", async () => {
      const { result } = renderMutationHook(() => useScaleNodegroup())

      result.current.mutate({
        clusterName: "c1",
        nodegroupName: "ng1",
        minSize: 2,
        maxSize: 5,
        desiredSize: 3,
      })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        nodegroupName: "ng1",
        scalingConfig: { minSize: 2, maxSize: 5, desiredSize: 3 },
      })
    })
  })

  describe("useDeleteNodegroup", () => {
    it("sends DeleteNodegroupCommand", async () => {
      const { result } = renderMutationHook(() => useDeleteNodegroup())

      result.current.mutate({ clusterName: "c1", nodegroupName: "ng1" })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        nodegroupName: "ng1",
      })
    })
  })

  describe("useCreateAddon", () => {
    it("maps params to CreateAddonCommand", async () => {
      const { result } = renderMutationHook(() => useCreateAddon())

      result.current.mutate({
        clusterName: "c1",
        addonName: "coredns",
        addonVersion: "v1.0.0",
        serviceAccountRoleArn: "arn:role",
        configurationValues: '{"replicaCount":2}',
      })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
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
      const { result } = renderMutationHook(() => useUpdateAddon())

      result.current.mutate({
        clusterName: "c1",
        addonName: "coredns",
        addonVersion: "v2.0.0",
      })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
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
      const { result } = renderMutationHook(() => useDeleteAddon())

      result.current.mutate({ clusterName: "c1", addonName: "coredns" })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        addonName: "coredns",
      })
    })
  })

  describe("useCreateAccessEntry", () => {
    it("sends CreateAccessEntryCommand with kubernetes groups", async () => {
      const { result } = renderMutationHook(() => useCreateAccessEntry())

      result.current.mutate({
        clusterName: "c1",
        principalArn: "arn:p",
        kubernetesGroups: ["system:masters"],
      })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
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
      const { result } = renderMutationHook(() => useAssociateAccessPolicy())

      result.current.mutate({
        clusterName: "c1",
        principalArn: "arn:p",
        policyArn: "arn:policy",
        accessScopeType: "cluster",
      })

      await waitFor(() => {
        expect(result.current.isSuccess).toBeTruthy()
      })
      expect(mockSend.mock.calls[0]?.[0].input).toStrictEqual({
        clusterName: "c1",
        principalArn: "arn:p",
        policyArn: "arn:policy",
        accessScope: { type: "cluster", namespaces: undefined },
      })
    })
  })
})
