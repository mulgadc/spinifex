import type { TransitGatewayVpcAttachment } from "@aws-sdk/client-ec2"
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
import { useDeleteTransitGatewayVpcAttachment } from "@/mutations/ec2"

import { CreateTransitGatewayVpcAttachmentPage } from "./create-transit-gateway-vpc-attachment-page"
import { TransitGatewayVpcAttachmentDetails } from "./transit-gateway-vpc-attachment-details"

const COLUMNS: DataColumn<TransitGatewayVpcAttachment>[] = [
  {
    id: "attachment",
    header: "Attachment",
    sortValue: (att) => getNameTag(att.Tags) ?? att.TransitGatewayAttachmentId,
    cell: (att) => {
      const name = getNameTag(att.Tags)
      const id = att.TransitGatewayAttachmentId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-transit-gateway-vpc-attachments/"
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
    sortValue: (att) => att.State,
    cell: (att) => <StateBadge state={att.State} />,
  },
  {
    id: "gateway",
    header: "Transit gateway",
    sortValue: (att) => att.TransitGatewayId,
    hideBelow: "sm",
    cell: (att) =>
      att.TransitGatewayId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: att.TransitGatewayId }}
          to="/ec2/describe-transit-gateways/$id"
        >
          {att.TransitGatewayId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (att) => att.VpcId,
    hideBelow: "md",
    cell: (att) =>
      att.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: att.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {att.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "subnets",
    header: "Subnets",
    align: "end",
    sortValue: (att) => att.SubnetIds?.length ?? 0,
    hideBelow: "lg",
    cell: (att) => att.SubnetIds?.length ?? 0,
  },
]

interface TransitGatewayVpcAttachmentsPageProps {
  attachments: readonly TransitGatewayVpcAttachment[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function TransitGatewayVpcAttachmentsPage({
  attachments,
  search,
  onSearchChange,
}: TransitGatewayVpcAttachmentsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = attachments.find(
    (att) =>
      search.selected !== "" &&
      att.TransitGatewayAttachmentId === search.selected,
  )

  const selection = useRowSelection(
    attachments.map((att) => att.TransitGatewayAttachmentId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteAttachment = useDeleteTransitGatewayVpcAttachment()

  const names = new Map(
    attachments.map((att) => [
      att.TransitGatewayAttachmentId,
      getNameTag(att.Tags),
    ]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        title="Create VPC attachment"
      >
        <CreateTransitGatewayVpcAttachmentPage
          onClose={(id) => {
            onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.TransitGatewayAttachmentId) {
    panel = (
      <TransitGatewayVpcAttachmentDetails
        attachment={open}
        key={open.TransitGatewayAttachmentId}
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
            from="/ec2/describe-transit-gateway-vpc-attachments/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create VPC attachment
          </Link>
        }
        subtitle="Connect a VPC to a transit gateway through one subnet in each Availability Zone."
        title="Transit Gateway Attachments"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteAttachment.mutateAsync}
            description="Traffic between each VPC and its transit gateway stops, and routes to the gateway from that VPC become blackholes. This cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["attachment", "attachments"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.TransitGatewayAttachmentId}
        caption="Transit gateway attachments"
        columns={COLUMNS}
        empty="No transit gateway attachments yet."
        getRowId={(att) => att.TransitGatewayAttachmentId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={attachments}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
