import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import { CreatePanel } from "@/components/create-panel"
import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { SelectionBar } from "@/components/selection-bar"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteLoadBalancer } from "@/mutations/elbv2"

import { CreateLoadBalancerPage } from "./create-load-balancer-page"
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

  const selection = useRowSelection(
    loadBalancers.map((lb) => lb.LoadBalancerArn ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteLoadBalancer = useDeleteLoadBalancer()

  const names = new Map(
    loadBalancers.map((lb) => [lb.LoadBalancerArn, lb.LoadBalancerName]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel onClose={closePanel} title="Create load balancer">
        <CreateLoadBalancerPage
          onClose={(arn) => {
            onSearchChange({ ...CLOSED_PANEL, selected: arn ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.LoadBalancerArn) {
    panel = (
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

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/ec2/describe-load-balancers/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create load balancer
          </Link>
        }
        subtitle="Spread traffic across targets in a VPC."
        title="Load Balancers"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteLoadBalancer.mutateAsync}
            description="This cannot be undone."
            ids={selection.ids}
            label={(id) => names.get(id) ?? id}
            noun={["load balancer", "load balancers"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.LoadBalancerArn}
        caption="Load balancers"
        columns={COLUMNS}
        empty="No load balancers yet."
        getRowId={(lb) => lb.LoadBalancerArn ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={loadBalancers}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
