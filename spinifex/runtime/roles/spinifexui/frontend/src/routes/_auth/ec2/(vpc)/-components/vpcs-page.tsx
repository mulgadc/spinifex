import type { Vpc } from "@aws-sdk/client-ec2"
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
import { useDeleteVpc } from "@/mutations/ec2"

import { CreateVpcPage } from "./create-vpc-page"
import { VpcDetails } from "./vpc-details"

const COLUMNS: DataColumn<Vpc>[] = [
  {
    id: "vpc",
    header: "VPC",
    sortValue: (vpc) => getNameTag(vpc.Tags) ?? vpc.VpcId,
    cell: (vpc) => {
      const name = getNameTag(vpc.Tags)
      const id = vpc.VpcId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-vpcs/"
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
    sortValue: (vpc) => vpc.State,
    cell: (vpc) => <StateBadge state={vpc.State} />,
  },
  {
    id: "cidr",
    header: "CIDR",
    sortValue: (vpc) => vpc.CidrBlock,
    hideBelow: "sm",
    cell: (vpc) => (
      <span className="font-mono text-xs tabular-nums">{vpc.CidrBlock}</span>
    ),
  },
  {
    id: "default",
    header: "Default",
    sortValue: (vpc) => (vpc.IsDefault ? "yes" : "no"),
    hideBelow: "md",
    cell: (vpc) => (vpc.IsDefault ? "Yes" : "No"),
  },
  {
    id: "tenancy",
    header: "Tenancy",
    sortValue: (vpc) => vpc.InstanceTenancy,
    hideBelow: "lg",
    cell: (vpc) => vpc.InstanceTenancy ?? "—",
  },
]

interface VpcsPageProps {
  vpcs: readonly Vpc[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function VpcsPage({ vpcs, search, onSearchChange }: VpcsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = vpcs.find(
    (vpc) => search.selected !== "" && vpc.VpcId === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const selection = useRowSelection(
    vpcs.map((vpc) => vpc.VpcId ?? ""),
    open !== undefined,
    closePanel,
  )

  const deleteVpc = useDeleteVpc()

  const names = new Map(vpcs.map((vpc) => [vpc.VpcId, getNameTag(vpc.Tags)]))

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel onClose={closePanel} title="Create VPC">
        <CreateVpcPage
          onClose={(vpcId) => {
            onSearchChange({ ...CLOSED_PANEL, selected: vpcId ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.VpcId) {
    panel = (
      <VpcDetails
        key={open.VpcId}
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        tab={search.tab}
        vpc={open}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/ec2/describe-vpcs/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create VPC
          </Link>
        }
        subtitle="Isolated networks for your instances."
        title="VPCs"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteVpc.mutateAsync}
            description="Each must have no subnets or internet gateways left. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["VPC", "VPCs"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.VpcId}
        caption="VPCs"
        columns={COLUMNS}
        empty="No VPCs yet."
        getRowId={(vpc) => vpc.VpcId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={vpcs}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
