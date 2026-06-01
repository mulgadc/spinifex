import { queryOptions } from "@tanstack/react-query"

import { getCredentials } from "@/lib/auth"
import { signedFetch } from "@/lib/signed-fetch"

interface InstanceTypeCap {
  name: string
  vcpu: number
  memory_gb: number
  available: number
}

interface GPUSliceInfo {
  gi_id: number
  profile: string
  vram_mib: number
  mdev_path: string
  instance_id?: string
}

interface GPUInfo {
  pci_address: string
  model: string
  vram_mib: number
  mig_enabled: boolean
  mig_profile?: string
  instance_id?: string
  slices?: GPUSliceInfo[]
}

interface VMGPUInfo {
  model: string
  vram_mib: number
  pci_address?: string
  profile?: string
  mdev_path?: string
}

interface NodeInfo {
  node: string
  status: string
  host: string
  region: string
  az: string
  uptime: number
  services: string[]
  vm_count: number
  total_vcpu: number
  total_mem_gb: number
  alloc_vcpu: number
  alloc_mem_gb: number
  total_gpus: number
  alloc_gpus: number
  instance_types: InstanceTypeCap[]
  gpus?: GPUInfo[]
  nats_role?: string
  predastore_role?: string
}

interface GetNodesOutput {
  nodes: NodeInfo[]
  cluster_mode: string
}

interface VMInfo {
  instance_id: string
  status: string
  instance_type: string
  vcpu: number
  memory_gb: number
  node: string
  launch_time: number
  managed_by?: string
  gpu?: VMGPUInfo
}

interface GetVMsOutput {
  vms: VMInfo[]
}

interface DBNodeStatus {
  id: number
  host: string
  port: number
  healthy: boolean
  state?: string
  leader?: string
  leader_addr?: string
  term?: string
  commit_index?: string
  applied_index?: string
  is_leader: boolean
}

interface ShardNode {
  id: number
  host: string
  port: number
}

interface StorageBucket {
  name: string
  type: string
  region: string
}

interface StorageStatusOutput {
  encoding: {
    type: string
    data_shards: number
    parity_shards: number
  }
  db_nodes: DBNodeStatus[]
  shard_nodes: ShardNode[]
  buckets: StorageBucket[]
}

export type {
  InstanceTypeCap,
  GPUSliceInfo,
  GPUInfo,
  VMGPUInfo,
  NodeInfo,
  GetNodesOutput,
  VMInfo,
  GetVMsOutput,
  DBNodeStatus,
  ShardNode,
  StorageBucket,
  StorageStatusOutput,
}

export const adminNodesQueryOptions = queryOptions({
  queryKey: ["admin", "nodes"],
  queryFn: async () => {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("Not authenticated")
    }
    return await signedFetch<GetNodesOutput>({
      action: "GetNodes",
      credentials,
    })
  },
  staleTime: 10_000,
})

export const adminVMsQueryOptions = queryOptions({
  queryKey: ["admin", "vms"],
  queryFn: async () => {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("Not authenticated")
    }
    return await signedFetch<GetVMsOutput>({
      action: "GetVMs",
      credentials,
    })
  },
  staleTime: 10_000,
})

export const adminStorageStatusQueryOptions = queryOptions({
  queryKey: ["admin", "storageStatus"],
  queryFn: async () => {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("Not authenticated")
    }
    return await signedFetch<StorageStatusOutput>({
      action: "GetStorageStatus",
      credentials,
    })
  },
  staleTime: 10_000,
})
