import type { VpcEndpoint } from "@aws-sdk/client-ec2"
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
import { getNameTag } from "@/lib/utils"
import { useDeleteVpcEndpoints } from "@/mutations/ec2"

import { CreateVpcEndpointPage } from "./create-vpc-endpoint-page"
import { VpcEndpointDetails } from "./vpc-endpoint-details"

const COLUMNS: DataColumn<VpcEndpoint>[] = [
  {
    id: "endpoint",
    header: "Endpoint",
    sortValue: (e) => getNameTag(e.Tags) ?? e.VpcEndpointId,
    cell: (e) => {
      const name = getNameTag(e.Tags)
      const id = e.VpcEndpointId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-vpc-endpoints/"
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
    id: "status",
    header: "Status",
    sortValue: (e) => e.State,
    cell: (e) => <StateBadge state={e.State} />,
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (e) => e.VpcId,
    hideBelow: "sm",
    cell: (e) =>
      e.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: e.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {e.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "service",
    header: "Service name",
    sortValue: (e) => e.ServiceName,
    hideBelow: "md",
    cell: (e) => <span className="font-mono text-xs">{e.ServiceName}</span>,
  },
  {
    id: "type",
    header: "Type",
    sortValue: (e) => e.VpcEndpointType,
    hideBelow: "md",
    cell: (e) => e.VpcEndpointType,
  },
  {
    id: "routeTables",
    header: "Route tables",
    align: "end",
    sortValue: (e) => e.RouteTableIds?.length ?? 0,
    hideBelow: "lg",
    cell: (e) => e.RouteTableIds?.length ?? 0,
  },
]

interface VpcEndpointsPageProps {
  endpoints: readonly VpcEndpoint[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function VpcEndpointsPage({
  endpoints,
  search,
  onSearchChange,
}: VpcEndpointsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = endpoints.find(
    (e) => search.selected !== "" && e.VpcEndpointId === search.selected,
  )

  const selection = useRowSelection(
    endpoints.map((e) => e.VpcEndpointId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteEndpoints = useDeleteVpcEndpoints()

  const names = new Map(
    endpoints.map((e) => [e.VpcEndpointId, getNameTag(e.Tags)]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        title="Create endpoint"
      >
        <CreateVpcEndpointPage
          onClose={(id) => {
            onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.VpcEndpointId) {
    panel = (
      <VpcEndpointDetails
        endpoint={open}
        key={open.VpcEndpointId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
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
            from="/ec2/describe-vpc-endpoints/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create endpoint
          </Link>
        }
        subtitle="Reach S3 from private subnets through their route tables, with no internet or NAT gateway."
        title="Endpoints"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteMany={deleteEndpoints.mutateAsync}
            description="Their routes leave every route table they serve, and subnets that used them lose that path to the service. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["endpoint", "endpoints"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.VpcEndpointId}
        caption="Endpoints"
        columns={COLUMNS}
        empty="No endpoints yet."
        getRowId={(e) => e.VpcEndpointId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={endpoints}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
