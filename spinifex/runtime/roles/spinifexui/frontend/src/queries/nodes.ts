import { queryOptions } from "@tanstack/react-query"

import { signedFetch } from "@/lib/signed-fetch"

// These mirror the daemon's NodeFacts, which is snake_case on the wire.
export interface SystemFacts {
  vendor?: string
  product?: string
  family?: string
  version?: string
  serial?: string
  uuid?: string
  chassis_type?: string
  chassis_asset?: string
  board_vendor?: string
  board_product?: string
  board_serial?: string
  bios_vendor?: string
  bios_version?: string
  bios_date?: string
  device_model?: string
}

export interface OSFacts {
  name?: string
  kernel?: string
  arch?: string
  spx_version?: string
  spx_commit?: string
  uptime_seconds?: number
  hostname?: string
}

export interface CPUFacts {
  model?: string
  vendor?: string
  sockets: number
  cores_per_socket?: number
  cores: number
  threads: number
  max_mhz?: number
  current_mhz?: number
  virtualization?: string
  features?: string[]
  flags?: string[]
}

export interface MemoryGroup {
  count: number
  size_bytes: number
  type?: string
  type_detail?: string
  form_factor?: string
  speed_mts?: number
  configured_mts?: number
  manufacturer?: string
  part_number?: string
  rank?: string
  serials?: string[]
}

export interface MemoryFacts {
  total_bytes: number
  slots_used: number
  slots_total: number
  max_capacity?: string
  ecc?: string
  groups: MemoryGroup[] | null
  os_total_bytes?: number
  os_available_bytes?: number
  huge_page_bytes?: number
}

export interface LLDPNeighbor {
  chassis_id: string
  system_name?: string
  system_description?: string
  port_id?: string
  port_description?: string
  capabilities?: string[]
}

export interface NICFacts {
  name: string
  mac?: string
  driver?: string
  firmware?: string
  bus_info?: string
  model?: string
  speed_mbps?: number
  max_mbps?: number
  duplex?: string
  port?: string
  mtu?: number
  state: string
  master?: string
  planes?: string[]
  neighbor?: LLDPNeighbor
}

export type Plane = "wan" | "lan" | "vpc"

export interface PlaneFacts {
  plane: Plane
  bridge?: string
  address?: string
  prefix?: number
  nics?: string[]
  mtu?: number
}

export type FormFactor =
  | "rack-server"
  | "tower"
  | "virtual-machine"
  | "sbc"
  | "jetson"
  | "unknown"

export interface NodeFacts {
  node: string
  collected_at?: string
  form_factor: FormFactor
  virt?: string
  system: SystemFacts
  os: OSFacts
  cpu: CPUFacts
  memory: MemoryFacts
  nics: NICFacts[] | null
  planes: PlaneFacts[] | null
  sources: Record<string, string> | null
}

interface GetNodeFactsOutput {
  nodes: NodeFacts[]
}

export interface NetworkTestRun {
  from: string
  to: string
  status: NetworkTestStatus
  source_address?: string
  bits_per_second: number
  bytes: number
  seconds: number
  streams: number
  stream_bps?: number[]
  rtt_min_ms?: number
  rtt_avg_ms?: number
  rtt_max_ms?: number
  packet_loss_percent?: number
  link_mbps?: number
  mtu?: number
  error?: string
}

export type NetworkTestStatus = "pending" | "running" | "complete" | "failed"

// The test record is camelCase, unlike the per-run measurements it holds.
export interface NetworkTest {
  testId: string
  plane: Plane
  status: NetworkTestStatus
  durationSeconds: number
  streams: number
  startedAt: string
  completedAt?: string
  error?: string
  runs: NetworkTestRun[]
}

interface DescribeNetworkTestsOutput {
  tests: NetworkTest[]
}

export const nodeFactsQueryOptions = queryOptions({
  queryKey: ["admin", "nodeFacts"],
  queryFn: async () =>
    await signedFetch<GetNodeFactsOutput>({ action: "GetNodeFacts" }),
  staleTime: 30_000,
})

export const networkTestsQueryOptions = queryOptions({
  queryKey: ["admin", "networkTests"],
  queryFn: async () =>
    await signedFetch<DescribeNetworkTestsOutput>({
      action: "DescribeNetworkTests",
    }),
  // A running test is polled until it settles; a settled list is left alone.
  refetchInterval: (query) =>
    query.state.data?.tests.some((test) => test.status === "running")
      ? 1500
      : false,
})

export interface StartNetworkTestInput {
  plane: Plane
  source?: string
  target?: string
  durationSeconds: number
  streams: number
}

export async function startNetworkTest(
  input: StartNetworkTestInput,
): Promise<NetworkTest> {
  return await signedFetch<NetworkTest>({
    action: "StartNetworkTest",
    params: {
      Plane: input.plane,
      DurationSeconds: String(input.durationSeconds),
      Streams: String(input.streams),
      ...(input.source && { Source: input.source }),
      ...(input.target && { Target: input.target }),
    },
  })
}
