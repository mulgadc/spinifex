import type { NatGateway } from "@aws-sdk/client-ec2"
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
import { useDeleteNatGateway } from "@/mutations/ec2"

import { CreateNatGatewayPage } from "./create-nat-gateway-page"
import { NatGatewayDetails } from "./nat-gateway-details"

const COLUMNS: DataColumn<NatGateway>[] = [
  {
    id: "gateway",
    header: "NAT gateway",
    sortValue: (nat) => getNameTag(nat.Tags) ?? nat.NatGatewayId,
    cell: (nat) => {
      const name = getNameTag(nat.Tags)
      const id = nat.NatGatewayId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-nat-gateways/"
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
    sortValue: (nat) => nat.State,
    cell: (nat) => <StateBadge state={nat.State} />,
  },
  {
    id: "publicIp",
    header: "Public IP",
    sortValue: (nat) => nat.NatGatewayAddresses?.[0]?.PublicIp,
    hideBelow: "sm",
    cell: (nat) => (
      <span className="font-mono text-xs">
        {nat.NatGatewayAddresses?.[0]?.PublicIp ?? "—"}
      </span>
    ),
  },
  {
    id: "subnet",
    header: "Subnet",
    sortValue: (nat) => nat.SubnetId,
    hideBelow: "md",
    cell: (nat) =>
      nat.SubnetId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: nat.SubnetId }}
          to="/ec2/describe-subnets/$id"
        >
          {nat.SubnetId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (nat) => nat.VpcId,
    hideBelow: "lg",
    cell: (nat) =>
      nat.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: nat.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {nat.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
]

interface NatGatewaysPageProps {
  natGateways: readonly NatGateway[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function NatGatewaysPage({
  natGateways,
  search,
  onSearchChange,
}: NatGatewaysPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = natGateways.find(
    (nat) => search.selected !== "" && nat.NatGatewayId === search.selected,
  )

  const selection = useRowSelection(
    natGateways.map((nat) => nat.NatGatewayId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteNatGateway = useDeleteNatGateway()

  const names = new Map(
    natGateways.map((nat) => [nat.NatGatewayId, getNameTag(nat.Tags)]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        title="Create NAT gateway"
      >
        <CreateNatGatewayPage
          onClose={(id) => {
            onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.NatGatewayId) {
    panel = (
      <NatGatewayDetails
        key={open.NatGatewayId}
        nat={open}
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
            from="/ec2/describe-nat-gateways/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create NAT gateway
          </Link>
        }
        subtitle="Let private subnets reach out without being reached."
        title="NAT gateways"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteNatGateway.mutateAsync}
            description="This cannot be undone. Their Elastic IPs are not released."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["NAT gateway", "NAT gateways"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.NatGatewayId}
        caption="NAT gateways"
        columns={COLUMNS}
        empty="No NAT gateways yet."
        getRowId={(nat) => nat.NatGatewayId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={natGateways}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
