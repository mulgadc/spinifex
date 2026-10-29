import type { InternetGateway } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { DetailPanel, PanelSpinner } from "@/components/detail-panel"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"

import { CreateInternetGatewayPage } from "./create-internet-gateway-page"
import {
  attachmentState,
  InternetGatewayDetails,
} from "./internet-gateway-details"

const COLUMNS: DataColumn<InternetGateway>[] = [
  {
    id: "gateway",
    header: "Internet gateway",
    sortValue: (igw) => getNameTag(igw.Tags) ?? igw.InternetGatewayId,
    cell: (igw) => {
      const name = getNameTag(igw.Tags)
      const id = igw.InternetGatewayId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-internet-gateways/"
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
    sortValue: attachmentState,
    cell: (igw) => <StateBadge state={attachmentState(igw)} />,
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (igw) => igw.Attachments?.[0]?.VpcId,
    hideBelow: "sm",
    cell: (igw) => {
      const vpcId = igw.Attachments?.[0]?.VpcId

      return vpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: vpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {vpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      )
    },
  },
  {
    id: "owner",
    header: "Owner",
    sortValue: (igw) => igw.OwnerId,
    hideBelow: "lg",
    cell: (igw) => (
      <span className="font-mono text-xs tabular-nums">{igw.OwnerId}</span>
    ),
  },
]

interface InternetGatewaysPageProps {
  internetGateways: readonly InternetGateway[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function InternetGatewaysPage({
  internetGateways,
  search,
  onSearchChange,
}: InternetGatewaysPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = internetGateways.find(
    (igw) =>
      search.selected !== "" && igw.InternetGatewayId === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <DetailPanel
        closeLabel="Close create form"
        label="Create internet gateway"
        onClose={closePanel}
        title="Create internet gateway"
      >
        <Suspense fallback={<PanelSpinner />}>
          <CreateInternetGatewayPage
            onClose={(igwId) => {
              onSearchChange({ ...CLOSED_PANEL, selected: igwId ?? "" })
            }}
          />
        </Suspense>
      </DetailPanel>
    )
  } else if (open?.InternetGatewayId) {
    panel = (
      <InternetGatewayDetails
        gateways={internetGateways}
        igw={open}
        key={open.InternetGatewayId}
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
            from="/ec2/describe-internet-gateways/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create internet gateway
          </Link>
        }
        subtitle="Connect a VPC to the outside world."
        title="Internet gateways"
      />
      <DataTable
        activeId={open?.InternetGatewayId}
        caption="Internet gateways"
        columns={COLUMNS}
        empty="No internet gateways yet."
        getRowId={(igw) => igw.InternetGatewayId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={internetGateways}
        sort={sort}
      />
    </ResourceView>
  )
}
