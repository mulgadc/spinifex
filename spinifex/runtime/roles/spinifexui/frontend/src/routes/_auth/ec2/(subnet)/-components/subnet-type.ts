import type { Subnet } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"

import { type SubnetType, subnetType } from "@/lib/subnet-routing"
import { ec2RouteTablesQueryOptions } from "@/queries/ec2"

// "Public" or "Private" by the route table, and "-" until it can be decided.
export const SUBNET_TYPE_LABELS: Record<SubnetType, string> = {
  public: "Public",
  private: "Private",
  unknown: "-",
}

// Route tables read on their own, so a failed read leaves the list intact.
// A failed refresh keeps its last data, which no longer decides anything.
export function useSubnetType(): (subnet: Subnet) => SubnetType {
  const { data, isError } = useQuery(ec2RouteTablesQueryOptions)
  const tables = isError ? undefined : data?.RouteTables

  return (subnet) => subnetType(subnet, tables, data?.NextToken === undefined)
}
