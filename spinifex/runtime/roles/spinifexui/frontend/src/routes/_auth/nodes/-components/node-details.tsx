import { Activity, Eye, Gauge, Network } from "lucide-react"

import { DetailPanel } from "@/components/detail-panel"
import { StateBadge } from "@/components/state-badge"
import type { PanelTab } from "@/lib/panel-search"
import type { NodeInfo } from "@/queries/admin"
import type { NodeFacts } from "@/queries/nodes"

import { NodeHardware } from "./node-hardware"
import { NodeMetrics } from "./node-metrics"
import { NodeNetwork } from "./node-network"
import { NodeNetworkTest } from "./node-network-test"

interface NodeDetailsProps {
  node: NodeInfo
  facts: NodeFacts | undefined
  peers: readonly string[]
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  onClose: () => void
}

// One cluster node: what it is, how it is wired, how busy it is, and how fast
// it can talk to its peers.
export function NodeDetails({
  node,
  facts,
  peers,
  tab,
  onTabChange,
  onClose,
}: NodeDetailsProps) {
  const model = facts
    ? [facts.system.vendor, facts.system.product].filter(Boolean).join(" ")
    : ""

  return (
    <DetailPanel
      closeLabel="Close node details"
      label="Node details"
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={model || node.host}
      summary={<StateBadge state={node.status} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Hardware",
          icon: Eye,
          content: <NodeHardware facts={facts} />,
        },
        {
          id: "network",
          label: "Network",
          icon: Network,
          content: <NodeNetwork facts={facts} />,
        },
        {
          id: "monitoring",
          label: "Metrics",
          icon: Activity,
          content: <NodeMetrics node={node.node} />,
        },
        {
          id: "speedtest",
          label: "Network test",
          icon: Gauge,
          content: (
            <NodeNetworkTest facts={facts} node={node.node} peers={peers} />
          ),
        },
      ]}
      title={node.node}
    />
  )
}
