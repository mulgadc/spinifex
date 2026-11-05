import type { Instance } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Workflow } from "lucide-react"
import { type ReactNode, useState } from "react"

import type { PanelTabSpec } from "@/components/detail-panel"
import { NetworkTopology } from "@/components/topology/network-topology"
import { DirectionToggle } from "@/components/topology/topology-canvas"
import type { MapFocus } from "@/lib/network-map"
import type { LayoutDirection } from "@/lib/node-topology"
import type { PanelTab } from "@/lib/panel-search"
import { ec2VpcQueryOptions } from "@/queries/ec2"

interface ResourceTopologyProps {
  vpcId: string | undefined
  focus: MapFocus
  direction: LayoutDirection
  // Said when the resource belongs to no VPC, in place of a map.
  noVpc: string
  pin?: Instance
}

// A resource's Topology tab: its whole VPC, with the resource highlighted.
export function ResourceTopology({
  vpcId,
  focus,
  direction,
  noVpc,
  pin,
}: ResourceTopologyProps) {
  const { data } = useQuery({
    ...ec2VpcQueryOptions(vpcId ?? ""),
    enabled: vpcId !== undefined,
  })

  if (vpcId === undefined) {
    return <p className="text-sm text-muted-foreground">{noVpc}</p>
  }

  // The map draws from the VPC's ID; its name follows once it has read.
  const vpc = data?.Vpcs?.[0] ?? { VpcId: vpcId }

  return (
    <NetworkTopology direction={direction} focus={focus} pin={pin} vpc={vpc} />
  )
}

export interface TopologyTab {
  toggle: ReactNode
  spec: PanelTabSpec
}

interface TopologyTabOptions extends Omit<ResourceTopologyProps, "direction"> {
  page: boolean
  tab: PanelTab
}

// The Topology tab for a resource's panel, and the direction toggle its own
// page shows in the header while that tab is open.
export function useTopologyTab({
  page,
  tab,
  ...props
}: TopologyTabOptions): TopologyTab {
  const [direction, setDirection] = useState<LayoutDirection>("down")

  return {
    toggle: page && tab === "topology" && (
      <DirectionToggle onChange={setDirection} value={direction} />
    ),
    spec: {
      id: "topology",
      label: "Topology",
      icon: Workflow,
      content: (
        <ResourceTopology direction={page ? direction : "down"} {...props} />
      ),
    },
  }
}
