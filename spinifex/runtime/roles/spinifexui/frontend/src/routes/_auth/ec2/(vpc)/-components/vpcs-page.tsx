import type { Vpc } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { DetailPanel } from "@/components/detail-panel"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"

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

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <DetailPanel
        closeLabel="Close create form"
        label="Create VPC"
        onClose={closePanel}
        size="wide"
        title="Create VPC"
      >
        <Suspense fallback={<Skeleton className="h-96" />}>
          <CreateVpcPage
            onClose={(vpcId) => {
              onSearchChange({ ...CLOSED_PANEL, selected: vpcId ?? "" })
            }}
          />
        </Suspense>
      </DetailPanel>
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
      <DataTable
        activeId={open?.VpcId}
        caption="VPCs"
        columns={COLUMNS}
        empty="No VPCs yet."
        getRowId={(vpc) => vpc.VpcId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={vpcs}
        sort={sort}
      />
    </ResourceView>
  )
}
