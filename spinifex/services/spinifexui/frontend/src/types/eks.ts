import type { AMITypes, CapacityTypes } from "@aws-sdk/client-eks"

export interface CreateClusterFormData {
  name: string
  version: string
  roleArn: string
  subnetIds: string[]
  securityGroupIds: string[]
  bootstrapClusterCreatorAdminPermissions: boolean
}

export interface CreateNodegroupFormData {
  clusterName: string
  nodegroupName: string
  nodeRole: string
  subnetIds: string[]
  instanceTypes: string[]
  amiType?: AMITypes
  capacityType?: CapacityTypes
  diskSize?: number
  minSize: number
  maxSize: number
  desiredSize: number
}

export interface ScaleNodegroupParams {
  clusterName: string
  nodegroupName: string
  minSize: number
  maxSize: number
  desiredSize: number
}

export interface CreateAccessEntryFormData {
  clusterName: string
  principalArn: string
  kubernetesGroups: string[]
  type?: string
}

export interface AccessEntryParams {
  clusterName: string
  principalArn: string
}

export interface AssociateAccessPolicyParams {
  clusterName: string
  principalArn: string
  policyArn: string
  accessScopeType: "cluster" | "namespace"
  namespaces?: string[]
}

export interface DisassociateAccessPolicyParams {
  clusterName: string
  principalArn: string
  policyArn: string
}
