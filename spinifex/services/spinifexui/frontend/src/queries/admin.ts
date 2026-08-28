import { queryOptions } from "@tanstack/react-query"

import { getCredentials } from "@/lib/auth"
import { signedAdminFetch, signedFetch } from "@/lib/signed-fetch"

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
  reserved_vcpu: number
  reserved_mem_gb: number
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

interface MetaNodeStatus {
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

interface BlobNode {
  id: number
  host: string
  port: number
}

interface StorageBucket {
  name: string
  region: string
}

interface StorageStatusOutput {
  encoding: {
    type: string
    data_shards: number
    parity_shards: number
  }
  meta_nodes: MetaNodeStatus[]
  blob_nodes: BlobNode[]
  buckets: StorageBucket[]
}

// CatalogAvailability mirrors the reasons AdminCatalog computes server-side:
// "available" means the model is servable to this account today, the other
// three are the specific reason it currently is not.
type CatalogAvailability =
  | "available"
  | "ungranted"
  | "no-weights-staged"
  | "no-credential"

interface AdminCatalogEntry {
  modelId: string
  modelName: string
  family: string
  inputModalities: string[]
  outputModalities: string[]
  responseStreamingSupported: boolean
  inputPriceMicroUsdPerMillion: number
  outputPriceMicroUsdPerMillion: number
  priceKnown: boolean
  minVramMib: number
  instanceType: string
  coServeGroup: string
  availability: CatalogAvailability
}

interface ListOchreCatalogOutput {
  entries: AdminCatalogEntry[]
}

// ModelAccessList and ModelAccessChange mirror the gateway's wire shape,
// which is PascalCase here unlike the catalog's camelCase.
interface ModelAccessList {
  AccountId: string
  ModelIds: string[]
}

interface ModelAccessChange {
  AccountId: string
  ModelId: string
}

// AccountSummary and ListAccountsResponse mirror the /admin/ListAccounts
// response, which — unlike the Action= surface — uses camelCase.
interface AccountSummary {
  accountId: string
  accountName: string
  status: string
  createdAt: string
}

interface ListAccountsResponse {
  accounts: AccountSummary[]
  count: number
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
  MetaNodeStatus,
  BlobNode,
  StorageBucket,
  StorageStatusOutput,
  CatalogAvailability,
  AdminCatalogEntry,
  ListOchreCatalogOutput,
  ModelAccessList,
  ModelAccessChange,
  AccountSummary,
  ListAccountsResponse,
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

export const adminOchreCatalogQueryOptions = queryOptions({
  queryKey: ["admin", "ochreCatalog"],
  queryFn: async () => {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("Not authenticated")
    }
    return await signedFetch<ListOchreCatalogOutput>({
      action: "ListOchreCatalog",
      credentials,
    })
  },
  staleTime: 10_000,
})

// adminListAccountsQueryOptions may 403 for a session that is not a
// long-lived IAM user in the super-admin account; retry: false so the page
// gives up fast and falls back to the validated free-text account id.
export const adminListAccountsQueryOptions = queryOptions({
  queryKey: ["admin", "accounts"],
  queryFn: async () => {
    const credentials = getCredentials()
    if (!credentials) {
      throw new Error("Not authenticated")
    }
    return await signedAdminFetch<ListAccountsResponse>({
      method: "ListAccounts",
      credentials,
      body: {},
    })
  },
  retry: false,
  staleTime: 30_000,
})

export const adminModelAccessQueryOptions = (accountId: string) =>
  queryOptions({
    queryKey: ["admin", "modelAccess", accountId],
    queryFn: async () => {
      const credentials = getCredentials()
      if (!credentials) {
        throw new Error("Not authenticated")
      }
      return await signedFetch<ModelAccessList>({
        action: "ListModelAccess",
        credentials,
        params: { AccountId: accountId },
      })
    },
    enabled: accountId !== "",
    staleTime: 10_000,
  })

interface ModelAccessMutationInput {
  accountId: string
  modelId: string
}

export async function grantModelAccess({
  accountId,
  modelId,
}: ModelAccessMutationInput): Promise<ModelAccessChange> {
  const credentials = getCredentials()
  if (!credentials) {
    throw new Error("Not authenticated")
  }
  return await signedFetch<ModelAccessChange>({
    action: "GrantModelAccess",
    credentials,
    params: { AccountId: accountId, ModelId: modelId },
  })
}

export async function revokeModelAccess({
  accountId,
  modelId,
}: ModelAccessMutationInput): Promise<ModelAccessChange> {
  const credentials = getCredentials()
  if (!credentials) {
    throw new Error("Not authenticated")
  }
  return await signedFetch<ModelAccessChange>({
    action: "RevokeModelAccess",
    credentials,
    params: { AccountId: accountId, ModelId: modelId },
  })
}
