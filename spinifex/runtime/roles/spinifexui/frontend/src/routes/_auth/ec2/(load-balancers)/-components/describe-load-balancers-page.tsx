import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"

import { LoadBalancerDetails } from "./load-balancer-details"

const COLUMNS: DataColumn<LoadBalancer>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (lb) => lb.LoadBalancerName,
    cell: (lb) => (
      <Link
        className="block max-w-72 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/ec2/describe-load-balancers/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: lb.LoadBalancerArn ?? "",
        })}
        to="."
      >
        {lb.LoadBalancerName ?? lb.LoadBalancerArn}
      </Link>
    ),
  },
  {
    id: "dns",
    header: "DNS name",
    sortValue: (lb) => lb.DNSName,
    hideBelow: "lg",
    cell: (lb) => (
      <span className="block max-w-80 truncate font-mono text-xs">
        {lb.DNSName}
      </span>
    ),
  },
  {
    id: "state",
    header: "State",
    sortValue: (lb) => lb.State?.Code,
    cell: (lb) => <StateBadge state={lb.State?.Code} />,
  },
  {
    id: "type",
    header: "Type",
    sortValue: (lb) => lb.Type,
    hideBelow: "sm",
    cell: (lb) => lb.Type,
  },
  {
    id: "scheme",
    header: "Scheme",
    sortValue: (lb) => lb.Scheme,
    hideBelow: "md",
    cell: (lb) => lb.Scheme,
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (lb) => lb.VpcId,
    hideBelow: "md",
    cell: (lb) =>
      lb.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: lb.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {lb.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "created",
    header: "Created",
    sortValue: (lb) => lb.CreatedTime?.getTime(),
    hideBelow: "xl",
    cell: (lb) => (
      <span className="text-xs">
        {lb.CreatedTime && formatDateTime(lb.CreatedTime)}
      </span>
    ),
  },
]

interface DescribeLoadBalancersPageProps {
  loadBalancers: readonly LoadBalancer[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function DescribeLoadBalancersPage({
  loadBalancers,
  search,
  onSearchChange,
}: DescribeLoadBalancersPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = loadBalancers.find(
    (lb) => search.selected !== "" && lb.LoadBalancerArn === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  return (
    <ResourceView
      panel={
        open?.LoadBalancerArn && (
          <LoadBalancerDetails
            key={open.LoadBalancerArn}
            lb={open}
            onClose={closePanel}
            onTabChange={(tab) => {
              onSearchChange({ tab })
            }}
            tab={search.tab}
          />
        )
      }
    >
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/ec2/create-load-balancer">
            <Plus data-icon="inline-start" />
            Create load balancer
          </Link>
        }
        subtitle="Spread traffic across targets in a VPC."
        title="Load balancers"
      />
      <DataTable
        activeId={open?.LoadBalancerArn}
        caption="Load balancers"
        columns={COLUMNS}
        empty="No load balancers yet."
        getRowId={(lb) => lb.LoadBalancerArn ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={loadBalancers}
        sort={sort}
      />
    </ResourceView>
  )
}
