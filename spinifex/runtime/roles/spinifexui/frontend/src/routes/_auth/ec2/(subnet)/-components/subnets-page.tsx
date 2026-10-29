import type { Subnet } from "@aws-sdk/client-ec2"
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
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useDeleteSubnet } from "@/mutations/ec2"

import { SubnetDetails } from "./subnet-details"

const COLUMNS: DataColumn<Subnet>[] = [
  {
    id: "subnet",
    header: "Subnet",
    sortValue: (subnet) => getNameTag(subnet.Tags) ?? subnet.SubnetId,
    cell: (subnet) => {
      const name = getNameTag(subnet.Tags)
      const id = subnet.SubnetId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-subnets/"
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
    sortValue: (subnet) => subnet.State,
    cell: (subnet) => <StateBadge state={subnet.State} />,
  },
  {
    id: "cidr",
    header: "CIDR",
    sortValue: (subnet) => subnet.CidrBlock,
    cell: (subnet) => (
      <span className="font-mono text-xs tabular-nums">{subnet.CidrBlock}</span>
    ),
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (subnet) => subnet.VpcId,
    hideBelow: "sm",
    cell: (subnet) =>
      subnet.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: subnet.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {subnet.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "zone",
    header: "Availability zone",
    sortValue: (subnet) => subnet.AvailabilityZone,
    hideBelow: "md",
    cell: (subnet) => subnet.AvailabilityZone,
  },
  {
    id: "available",
    header: "Available IPs",
    sortValue: (subnet) => subnet.AvailableIpAddressCount,
    hideBelow: "lg",
    align: "end",
    cell: (subnet) => (
      <span className="tabular-nums">{subnet.AvailableIpAddressCount}</span>
    ),
  },
]

interface SubnetsPageProps {
  subnets: readonly Subnet[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function SubnetsPage({
  subnets,
  search,
  onSearchChange,
}: SubnetsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = subnets.find(
    (subnet) => search.selected !== "" && subnet.SubnetId === search.selected,
  )

  const selection = useRowSelection(
    subnets.map((subnet) => subnet.SubnetId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteSubnet = useDeleteSubnet()

  const names = new Map(
    subnets.map((subnet) => [subnet.SubnetId, getNameTag(subnet.Tags)]),
  )

  let panel: ReactNode = null

  if (open?.SubnetId) {
    panel = (
      <SubnetDetails
        key={open.SubnetId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        subnet={open}
        tab={search.tab}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/ec2/create-subnet">
            <Plus data-icon="inline-start" />
            Create subnet
          </Link>
        }
        subtitle="Address ranges within a VPC, each in one availability zone."
        title="Subnets"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteSubnet.mutateAsync}
            description="Each must have no network interfaces left. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["subnet", "subnets"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.SubnetId}
        caption="Subnets"
        columns={COLUMNS}
        empty="No subnets yet."
        getRowId={(subnet) => subnet.SubnetId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={subnets}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
