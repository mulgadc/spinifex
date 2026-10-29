import type { RouteTable } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, useState } from "react"

import { BulkDelete } from "@/components/bulk-delete"
import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { SelectionBar } from "@/components/selection-bar"
import { Badge } from "@/components/ui/badge"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useDeleteRouteTable } from "@/mutations/ec2"

import { isMainRouteTable, RouteTableDetails } from "./route-table-details"

function subnetCount(rtb: RouteTable): number {
  return rtb.Associations?.filter((a) => a.SubnetId).length ?? 0
}

const COLUMNS: DataColumn<RouteTable>[] = [
  {
    id: "routeTable",
    header: "Route table",
    sortValue: (rtb) => getNameTag(rtb.Tags) ?? rtb.RouteTableId,
    cell: (rtb) => {
      const name = getNameTag(rtb.Tags)
      const id = rtb.RouteTableId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-route-tables/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {name ?? id}
          </span>
          {name && (
            <span className="block truncate font-mono text-xs text-muted-foreground">
              {id}
            </span>
          )}
        </Link>
      )
    },
  },
  {
    id: "main",
    header: "Main",
    sortValue: (rtb) => (isMainRouteTable(rtb) ? 1 : 0),
    cell: (rtb) =>
      isMainRouteTable(rtb) ? (
        <Badge variant="secondary">Main</Badge>
      ) : (
        <span className="text-muted-foreground">No</span>
      ),
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (rtb) => rtb.VpcId,
    hideBelow: "sm",
    cell: (rtb) =>
      rtb.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: rtb.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {rtb.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "subnets",
    header: "Subnets",
    sortValue: subnetCount,
    hideBelow: "md",
    align: "end",
    cell: (rtb) => <span className="tabular-nums">{subnetCount(rtb)}</span>,
  },
  {
    id: "routes",
    header: "Routes",
    sortValue: (rtb) => rtb.Routes?.length ?? 0,
    hideBelow: "lg",
    align: "end",
    cell: (rtb) => (
      <span className="tabular-nums">{rtb.Routes?.length ?? 0}</span>
    ),
  },
]

interface RouteTablesPageProps {
  routeTables: readonly RouteTable[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function RouteTablesPage({
  routeTables,
  search,
  onSearchChange,
}: RouteTablesPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = routeTables.find(
    (rtb) => search.selected !== "" && rtb.RouteTableId === search.selected,
  )

  const selection = useRowSelection(
    routeTables.map((rtb) => rtb.RouteTableId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteRouteTable = useDeleteRouteTable()

  const names = new Map(
    routeTables.map((rtb) => [rtb.RouteTableId, getNameTag(rtb.Tags)]),
  )

  let panel: ReactNode = null

  if (open?.RouteTableId) {
    panel = (
      <RouteTableDetails
        key={open.RouteTableId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        rtb={open}
        tab={search.tab}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/ec2/create-route-table">
            <Plus data-icon="inline-start" />
            Create route table
          </Link>
        }
        subtitle="Decide where traffic from each subnet goes."
        title="Route tables"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteRouteTable.mutateAsync}
            description="This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["route table", "route tables"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.RouteTableId}
        caption="Route tables"
        columns={COLUMNS}
        empty="No route tables yet."
        getRowId={(rtb) => rtb.RouteTableId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={routeTables}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
