import {
  type Address,
  DescribeAddressesCommand,
  DescribeInstancesCommand,
  DescribeInternetGatewaysCommand,
  DescribeNatGatewaysCommand,
  DescribeRouteTablesCommand,
  DescribeSubnetsCommand,
  type Filter,
  type Instance,
  type NatGateway,
  type Subnet,
} from "@aws-sdk/client-ec2"
import { DescribeLoadBalancersCommand } from "@aws-sdk/client-elastic-load-balancing-v2"
import { DescribeDBInstancesCommand } from "@aws-sdk/client-rds"
import { queryOptions } from "@tanstack/react-query"

import { getEc2Client, getElbv2Client, getRdsClient } from "@/lib/aws-client"
import type { MapResource, NetworkInputs } from "@/lib/network-map"

// The map reads at most this many pages of each resource type, each of a
// fixed size, so an account's size never sets how much it reads.
export const MAP_PAGE_LIMIT = 5
export const MAP_PAGE_SIZE = 100

interface Page<T> {
  items: T[]
  next?: string
}

interface Paged<T> {
  items: T[]
  // A continuation marker was left unread.
  incomplete: boolean
}

export async function readPages<T>(
  read: (marker: string | undefined) => Promise<Page<T>>,
  limit = MAP_PAGE_LIMIT,
): Promise<Paged<T>> {
  const items: T[] = []
  let marker: string | undefined

  for (let page = 0; page < limit; page++) {
    // Each page needs the marker the one before it returned.
    // oxlint-disable-next-line no-await-in-loop
    const result = await read(marker)

    items.push(...result.items)
    marker = result.next

    if (!marker) {
      return { items, incomplete: false }
    }
  }

  return { items, incomplete: true }
}

interface Gaps {
  incomplete: MapResource[]
  failed: MapResource[]
}

// A failed read draws nothing of its type, and says so.
function take<T>(
  name: MapResource,
  outcome: PromiseSettledResult<Paged<T>>,
  gaps: Gaps,
): T[] {
  if (outcome.status === "rejected") {
    gaps.failed.push(name)

    return []
  }

  if (outcome.value.incomplete) {
    gaps.incomplete.push(name)
  }

  return outcome.value.items
}

async function readVpc(vpcId: string) {
  const ec2 = getEc2Client()
  const inVpc: Filter[] = [{ Name: "vpc-id", Values: [vpcId] }]

  return await Promise.allSettled([
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeSubnetsCommand({
          Filters: inVpc,
          MaxResults: MAP_PAGE_SIZE,
          NextToken,
        }),
      )

      return { items: r.Subnets ?? [], next: r.NextToken }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeRouteTablesCommand({
          Filters: inVpc,
          MaxResults: MAP_PAGE_SIZE,
          NextToken,
        }),
      )

      return { items: r.RouteTables ?? [], next: r.NextToken }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeInternetGatewaysCommand({
          Filters: [{ Name: "attachment.vpc-id", Values: [vpcId] }],
          MaxResults: MAP_PAGE_SIZE,
          NextToken,
        }),
      )

      return { items: r.InternetGateways ?? [], next: r.NextToken }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeNatGatewaysCommand({
          Filter: inVpc,
          MaxResults: MAP_PAGE_SIZE,
          NextToken,
        }),
      )

      return { items: r.NatGateways ?? [], next: r.NextToken }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeInstancesCommand({
          Filters: inVpc,
          MaxResults: MAP_PAGE_SIZE,
          NextToken,
        }),
      )

      return {
        items: (r.Reservations ?? []).flatMap((res) => res.Instances ?? []),
        next: r.NextToken,
      }
    }),
    // Neither service filters by VPC, so the builder does.
    readPages(async (Marker) => {
      const r = await getElbv2Client().send(
        new DescribeLoadBalancersCommand({ Marker, PageSize: MAP_PAGE_SIZE }),
      )

      return { items: r.LoadBalancers ?? [], next: r.NextMarker }
    }),
    readPages(async (Marker) => {
      const r = await getRdsClient().send(
        new DescribeDBInstancesCommand({
          Marker,
          MaxRecords: MAP_PAGE_SIZE,
        }),
      )

      return { items: r.DBInstances ?? [], next: r.Marker }
    }),
  ])
}

// DescribeAddresses does not paginate and cannot filter by VPC, so it is asked
// only for what the map already holds, a page's worth of values at a time.
async function readAddresses(
  instances: Instance[],
  nats: NatGateway[],
): Promise<Paged<Address>> {
  const ec2 = getEc2Client()
  const asks: Filter[] = []

  const batch = (name: string, values: (string | undefined)[]) => {
    const ids = [...new Set(values.filter((v) => v !== undefined))]

    for (let i = 0; i < ids.length; i += MAP_PAGE_SIZE) {
      asks.push({ Name: name, Values: ids.slice(i, i + MAP_PAGE_SIZE) })
    }
  }

  batch(
    "instance-id",
    instances.map((i) => i.InstanceId),
  )
  batch(
    "allocation-id",
    nats.flatMap((n) =>
      (n.NatGatewayAddresses ?? []).map((a) => a.AllocationId),
    ),
  )

  const pages = await Promise.all(
    asks.slice(0, MAP_PAGE_LIMIT * 2).map(async (filter) => {
      const r = await ec2.send(
        new DescribeAddressesCommand({ Filters: [filter] }),
      )

      return r.Addresses ?? []
    }),
  )

  // An address held by both an instance and a NAT gateway is drawn once.
  const unique = new Map(
    pages.flat().map((a) => [a.AllocationId ?? a.PublicIp, a]),
  )

  return {
    items: [...unique.values()],
    incomplete: asks.length > MAP_PAGE_LIMIT * 2,
  }
}

// The panel's own instance is drawn even past the page limit, so its map is
// never without it; its subnet is read by ID when the VPC's read missed it.
async function pinned(
  pin: Instance | undefined,
  read: Instance[],
  subnets: Subnet[],
  gaps: Gaps,
): Promise<{ instances: Instance[]; subnets: Subnet[] }> {
  if (!pin?.InstanceId) {
    return { instances: read, subnets }
  }

  const instances = [
    ...read.filter((i) => i.InstanceId !== pin.InstanceId),
    pin,
  ]

  if (!pin.SubnetId || subnets.some((s) => s.SubnetId === pin.SubnetId)) {
    return { instances, subnets }
  }

  try {
    const r = await getEc2Client().send(
      new DescribeSubnetsCommand({ SubnetIds: [pin.SubnetId] }),
    )

    return { instances, subnets: [...subnets, ...(r.Subnets ?? [])] }
  } catch {
    // The map still draws, and says its subnets are not all there.
    gaps.incomplete.push("subnets")

    return { instances, subnets }
  }
}

// Every read is settled on its own, so one failing service leaves the rest of
// the map drawn and names what is missing.
export const networkMapQueryOptions = (vpcId: string, pin?: Instance) =>
  queryOptions({
    // The whole instance, so a change to its subnet or address reads again.
    queryKey: ["topology", "network", vpcId, pin ?? null],
    queryFn: async (): Promise<Omit<NetworkInputs, "vpc">> => {
      const [subnets, routeTables, igws, nats, instances, lbs, dbs] =
        await readVpc(vpcId)

      const gaps: Gaps = { incomplete: [], failed: [] }
      const natGateways = take("natGateways", nats, gaps)

      const mine = await pinned(
        pin,
        take("instances", instances, gaps),
        take("subnets", subnets, gaps),
        gaps,
      )

      const [addresses] = await Promise.allSettled([
        readAddresses(mine.instances, natGateways),
      ])

      return {
        subnets: mine.subnets,
        routeTables: take("routeTables", routeTables, gaps),
        internetGateways: take("internetGateways", igws, gaps),
        natGateways,
        addresses: take("addresses", addresses, gaps),
        instances: mine.instances,
        loadBalancers: take("loadBalancers", lbs, gaps),
        dbInstances: take("dbInstances", dbs, gaps),
        ...gaps,
      }
    },
    refetchInterval: 10_000,
  })
