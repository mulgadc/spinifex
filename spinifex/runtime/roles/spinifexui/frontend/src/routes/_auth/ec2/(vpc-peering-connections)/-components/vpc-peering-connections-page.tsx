import type {
  VpcPeeringConnection,
  VpcPeeringConnectionVpcInfo,
} from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
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
import { useDeleteVpcPeeringConnection } from "@/mutations/ec2"
import { callerIdentityQueryOptions } from "@/queries/sts"

import { CreateVpcPeeringConnectionPage } from "./create-vpc-peering-connection-page"
import { VpcPeeringConnectionDetails } from "./vpc-peering-connection-details"

// A VPC the caller owns links to its page; another account's shows its owner.
export function PeeringVpc({
  info,
  accountId,
}: {
  info: VpcPeeringConnectionVpcInfo | undefined
  accountId: string | undefined
}) {
  if (!info?.VpcId) {
    return <span className="text-muted-foreground">—</span>
  }

  if (info.OwnerId !== undefined && info.OwnerId === accountId) {
    return (
      <Link
        className="font-mono text-xs text-primary hover:underline"
        params={{ id: info.VpcId }}
        to="/ec2/describe-vpcs/$id"
      >
        {info.VpcId}
      </Link>
    )
  }

  return (
    <span className="block font-mono text-xs">
      {info.VpcId}
      {info.OwnerId && (
        <span className="block text-muted-foreground">{info.OwnerId}</span>
      )}
    </span>
  )
}

function columns(
  accountId: string | undefined,
): DataColumn<VpcPeeringConnection>[] {
  return [
    {
      id: "connection",
      header: "Peering connection",
      sortValue: (pcx) => getNameTag(pcx.Tags) ?? pcx.VpcPeeringConnectionId,
      cell: (pcx) => {
        const name = getNameTag(pcx.Tags)
        const id = pcx.VpcPeeringConnectionId ?? ""

        return (
          <Link
            className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            from="/ec2/describe-vpc-peering-connections/"
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
      sortValue: (pcx) => pcx.Status?.Code,
      cell: (pcx) => <StateBadge state={pcx.Status?.Code} />,
    },
    {
      id: "requester",
      header: "Requester VPC",
      sortValue: (pcx) => pcx.RequesterVpcInfo?.VpcId,
      hideBelow: "sm",
      cell: (pcx) => (
        <PeeringVpc accountId={accountId} info={pcx.RequesterVpcInfo} />
      ),
    },
    {
      id: "accepter",
      header: "Accepter VPC",
      sortValue: (pcx) => pcx.AccepterVpcInfo?.VpcId,
      hideBelow: "sm",
      cell: (pcx) => (
        <PeeringVpc accountId={accountId} info={pcx.AccepterVpcInfo} />
      ),
    },
    {
      id: "requesterCidr",
      header: "Requester CIDR",
      sortValue: (pcx) => pcx.RequesterVpcInfo?.CidrBlock,
      hideBelow: "md",
      cell: (pcx) => (
        <span className="font-mono text-xs">
          {pcx.RequesterVpcInfo?.CidrBlock ?? "—"}
        </span>
      ),
    },
    {
      id: "accepterCidr",
      header: "Accepter CIDR",
      sortValue: (pcx) => pcx.AccepterVpcInfo?.CidrBlock,
      hideBelow: "lg",
      cell: (pcx) => (
        <span className="font-mono text-xs">
          {pcx.AccepterVpcInfo?.CidrBlock ?? "—"}
        </span>
      ),
    },
  ]
}

interface VpcPeeringConnectionsPageProps {
  connections: readonly VpcPeeringConnection[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function VpcPeeringConnectionsPage({
  connections,
  search,
  onSearchChange,
}: VpcPeeringConnectionsPageProps) {
  const [sort, setSort] = useState<SortState>()
  const { data: identity } = useQuery(callerIdentityQueryOptions)

  const open = connections.find(
    (pcx) =>
      search.selected !== "" && pcx.VpcPeeringConnectionId === search.selected,
  )

  const selection = useRowSelection(
    connections.map((pcx) => pcx.VpcPeeringConnectionId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteConnection = useDeleteVpcPeeringConnection()

  const names = new Map(
    connections.map((pcx) => [
      pcx.VpcPeeringConnectionId,
      getNameTag(pcx.Tags),
    ]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        title="Create peering connection"
      >
        <CreateVpcPeeringConnectionPage
          onClose={(id) => {
            onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.VpcPeeringConnectionId) {
    panel = (
      <VpcPeeringConnectionDetails
        key={open.VpcPeeringConnectionId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        pcx={open}
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
            from="/ec2/describe-vpc-peering-connections/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create peering connection
          </Link>
        }
        subtitle="Route privately between two VPCs, in this account or another."
        title="Peering Connections"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteConnection.mutateAsync}
            description="Traffic between the VPCs stops, and routes to these connections become blackholes. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["peering connection", "peering connections"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.VpcPeeringConnectionId}
        caption="Peering connections"
        columns={columns(identity?.Account)}
        empty="No peering connections yet."
        getRowId={(pcx) => pcx.VpcPeeringConnectionId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={connections}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
