import {
  AssociateAccessPolicyCommand,
  CreateAccessEntryCommand,
  CreateClusterCommand,
  CreateNodegroupCommand,
  DeleteAccessEntryCommand,
  DeleteClusterCommand,
  DeleteNodegroupCommand,
  DisassociateAccessPolicyCommand,
  UpdateNodegroupConfigCommand,
} from "@aws-sdk/client-eks"
import { useMutation, useQueryClient } from "@tanstack/react-query"

import { getEksClient } from "@/lib/awsClient"
import type {
  AccessEntryParams,
  AssociateAccessPolicyParams,
  CreateAccessEntryFormData,
  CreateClusterFormData,
  CreateNodegroupFormData,
  DisassociateAccessPolicyParams,
  ScaleNodegroupParams,
} from "@/types/eks"

export function useCreateCluster() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateClusterFormData) => {
      const command = new CreateClusterCommand({
        name: params.name,
        version: params.version,
        roleArn: params.roleArn,
        resourcesVpcConfig: {
          subnetIds: params.subnetIds,
          securityGroupIds: params.securityGroupIds,
        },
        accessConfig: {
          authenticationMode: "API",
          bootstrapClusterCreatorAdminPermissions:
            params.bootstrapClusterCreatorAdminPermissions,
        },
      })
      return await getEksClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["eks", "clusters"] })
    },
  })
}

export function useDeleteCluster() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (clusterName: string) => {
      const command = new DeleteClusterCommand({ name: clusterName })
      return await getEksClient().send(command)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["eks", "clusters"] })
    },
  })
}

export function useCreateNodegroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateNodegroupFormData) => {
      const command = new CreateNodegroupCommand({
        clusterName: params.clusterName,
        nodegroupName: params.nodegroupName,
        nodeRole: params.nodeRole,
        subnets: params.subnetIds,
        instanceTypes: params.instanceTypes,
        amiType: params.amiType,
        capacityType: params.capacityType,
        diskSize: params.diskSize,
        scalingConfig: {
          minSize: params.minSize,
          maxSize: params.maxSize,
          desiredSize: params.desiredSize,
        },
      })
      return await getEksClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["eks", "clusters", params.clusterName, "nodegroups"],
      })
    },
  })
}

export function useScaleNodegroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: ScaleNodegroupParams) => {
      const command = new UpdateNodegroupConfigCommand({
        clusterName: params.clusterName,
        nodegroupName: params.nodegroupName,
        scalingConfig: {
          minSize: params.minSize,
          maxSize: params.maxSize,
          desiredSize: params.desiredSize,
        },
      })
      return await getEksClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["eks", "clusters", params.clusterName, "nodegroups"],
      })
    },
  })
}

export function useDeleteNodegroup() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: {
      clusterName: string
      nodegroupName: string
    }) => {
      const command = new DeleteNodegroupCommand({
        clusterName: params.clusterName,
        nodegroupName: params.nodegroupName,
      })
      return await getEksClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["eks", "clusters", params.clusterName, "nodegroups"],
      })
    },
  })
}

export function useCreateAccessEntry() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: CreateAccessEntryFormData) => {
      const command = new CreateAccessEntryCommand({
        clusterName: params.clusterName,
        principalArn: params.principalArn,
        kubernetesGroups: params.kubernetesGroups,
        type: params.type,
      })
      return await getEksClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["eks", "clusters", params.clusterName, "access-entries"],
      })
    },
  })
}

export function useDeleteAccessEntry() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: AccessEntryParams) => {
      const command = new DeleteAccessEntryCommand({
        clusterName: params.clusterName,
        principalArn: params.principalArn,
      })
      return await getEksClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: ["eks", "clusters", params.clusterName, "access-entries"],
      })
    },
  })
}

export function useAssociateAccessPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: AssociateAccessPolicyParams) => {
      const command = new AssociateAccessPolicyCommand({
        clusterName: params.clusterName,
        principalArn: params.principalArn,
        policyArn: params.policyArn,
        accessScope: {
          type: params.accessScopeType,
          namespaces: params.namespaces,
        },
      })
      return await getEksClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: [
          "eks",
          "clusters",
          params.clusterName,
          "access-entries",
          params.principalArn,
          "policies",
        ],
      })
    },
  })
}

export function useDisassociateAccessPolicy() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (params: DisassociateAccessPolicyParams) => {
      const command = new DisassociateAccessPolicyCommand({
        clusterName: params.clusterName,
        principalArn: params.principalArn,
        policyArn: params.policyArn,
      })
      return await getEksClient().send(command)
    },
    onSuccess: (_data, params) => {
      void queryClient.invalidateQueries({
        queryKey: [
          "eks",
          "clusters",
          params.clusterName,
          "access-entries",
          params.principalArn,
          "policies",
        ],
      })
    },
  })
}
