import {
  DescribeAddressesCommand,
  DescribeInstancesCommand,
  DescribeInternetGatewaysCommand,
  DescribeNatGatewaysCommand,
  DescribeRouteTablesCommand,
  DescribeSubnetsCommand,
  type Filter,
} from "@aws-sdk/client-ec2"
import { DescribeLoadBalancersCommand } from "@aws-sdk/client-elastic-load-balancing-v2"
import { DescribeDBInstancesCommand } from "@aws-sdk/client-rds"
import { queryOptions } from "@tanstack/react-query"

import { getEc2Client, getElbv2Client, getRdsClient } from "@/lib/aws-client"
import type { MapResource, NetworkInputs } from "@/lib/network-map"

// The map reads at most this many pages of each resource type.
export const MAP_PAGE_LIMIT = 5

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
        new DescribeSubnetsCommand({ Filters: inVpc, NextToken }),
      )

      return { items: r.Subnets ?? [], next: r.NextToken }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeRouteTablesCommand({ Filters: inVpc, NextToken }),
      )

      return { items: r.RouteTables ?? [], next: r.NextToken }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeInternetGatewaysCommand({
          Filters: [{ Name: "attachment.vpc-id", Values: [vpcId] }],
          NextToken,
        }),
      )

      return { items: r.InternetGateways ?? [], next: r.NextToken }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeNatGatewaysCommand({ Filter: inVpc, NextToken }),
      )

      return { items: r.NatGateways ?? [], next: r.NextToken }
    }),
    // DescribeAddresses does not paginate.
    readPages(async () => {
      const r = await ec2.send(new DescribeAddressesCommand({}))

      return { items: r.Addresses ?? [] }
    }),
    readPages(async (NextToken) => {
      const r = await ec2.send(
        new DescribeInstancesCommand({ Filters: inVpc, NextToken }),
      )

      return {
        items: (r.Reservations ?? []).flatMap((res) => res.Instances ?? []),
        next: r.NextToken,
      }
    }),
    // Neither service filters by VPC, so the builder does.
    readPages(async (Marker) => {
      const r = await getElbv2Client().send(
        new DescribeLoadBalancersCommand({ Marker }),
      )

      return { items: r.LoadBalancers ?? [], next: r.NextMarker }
    }),
    readPages(async (Marker) => {
      const r = await getRdsClient().send(
        new DescribeDBInstancesCommand({ Marker }),
      )

      return { items: r.DBInstances ?? [], next: r.Marker }
    }),
  ])
}

// Every read is settled on its own, so one failing service leaves the rest of
// the map drawn and names what is missing.
export const networkMapQueryOptions = (vpcId: string) =>
  queryOptions({
    queryKey: ["topology", "network", vpcId],
    queryFn: async (): Promise<Omit<NetworkInputs, "vpc">> => {
      const [subnets, routeTables, igws, nats, addresses, instances, lbs, dbs] =
        await readVpc(vpcId)

      const gaps: Gaps = { incomplete: [], failed: [] }

      return {
        subnets: take("subnets", subnets, gaps),
        routeTables: take("routeTables", routeTables, gaps),
        internetGateways: take("internetGateways", igws, gaps),
        natGateways: take("natGateways", nats, gaps),
        addresses: take("addresses", addresses, gaps),
        instances: take("instances", instances, gaps),
        loadBalancers: take("loadBalancers", lbs, gaps),
        dbInstances: take("dbInstances", dbs, gaps),
        ...gaps,
      }
    },
    refetchInterval: 10_000,
  })
