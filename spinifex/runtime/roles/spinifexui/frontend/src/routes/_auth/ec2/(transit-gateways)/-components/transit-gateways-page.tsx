import type { TransitGateway } from "@aws-sdk/client-ec2"
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
import { TruncatedText } from "@/components/table-cells"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useDeleteTransitGateway } from "@/mutations/ec2"

import { CreateTransitGatewayPage } from "./create-transit-gateway-page"
import { TransitGatewayDetails } from "./transit-gateway-details"

const COLUMNS: DataColumn<TransitGateway>[] = [
  {
    id: "gateway",
    header: "Transit gateway",
    sortValue: (tgw) => getNameTag(tgw.Tags) ?? tgw.TransitGatewayId,
    cell: (tgw) => {
      const name = getNameTag(tgw.Tags)
      const id = tgw.TransitGatewayId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-transit-gateways/"
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
    id: "state",
    header: "State",
    sortValue: (tgw) => tgw.State,
    cell: (tgw) => <StateBadge state={tgw.State} />,
  },
  {
    id: "description",
    header: "Description",
    sortValue: (tgw) => tgw.Description,
    hideBelow: "sm",
    cell: (tgw) => (
      <TruncatedText className="max-w-48" text={tgw.Description} />
    ),
  },
  {
    id: "asn",
    header: "Amazon ASN",
    sortValue: (tgw) => tgw.Options?.AmazonSideAsn,
    hideBelow: "md",
    cell: (tgw) => (
      <span className="font-mono text-xs">
        {tgw.Options?.AmazonSideAsn ?? "—"}
      </span>
    ),
  },
  {
    id: "owner",
    header: "Owner",
    sortValue: (tgw) => tgw.OwnerId,
    hideBelow: "lg",
    cell: (tgw) => (
      <span className="font-mono text-xs">{tgw.OwnerId ?? "—"}</span>
    ),
  },
]

interface TransitGatewaysPageProps {
  transitGateways: readonly TransitGateway[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function TransitGatewaysPage({
  transitGateways,
  search,
  onSearchChange,
}: TransitGatewaysPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = transitGateways.find(
    (tgw) => search.selected !== "" && tgw.TransitGatewayId === search.selected,
  )

  const selection = useRowSelection(
    transitGateways.map((tgw) => tgw.TransitGatewayId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteTransitGateway = useDeleteTransitGateway()

  const names = new Map(
    transitGateways.map((tgw) => [tgw.TransitGatewayId, getNameTag(tgw.Tags)]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        title="Create transit gateway"
      >
        <CreateTransitGatewayPage
          onClose={(id) => {
            onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.TransitGatewayId) {
    panel = (
      <TransitGatewayDetails
        key={open.TransitGatewayId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        tab={search.tab}
        tgw={open}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/ec2/describe-transit-gateways/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create transit gateway
          </Link>
        }
        subtitle="Route between many VPCs through one hub, instead of a peering connection for each pair."
        title="Transit Gateways"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteTransitGateway.mutateAsync}
            description="A transit gateway with attachments is refused; delete its attachments first. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["transit gateway", "transit gateways"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.TransitGatewayId}
        caption="Transit gateways"
        columns={COLUMNS}
        empty="No transit gateways yet."
        getRowId={(tgw) => tgw.TransitGatewayId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={transitGateways}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
