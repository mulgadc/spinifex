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
export function useSubnetType(): (subnet: Subnet) => SubnetType {
  const { data } = useQuery(ec2RouteTablesQueryOptions)

  return (subnet) =>
    subnetType(subnet, data?.RouteTables, data?.NextToken === undefined)
}
