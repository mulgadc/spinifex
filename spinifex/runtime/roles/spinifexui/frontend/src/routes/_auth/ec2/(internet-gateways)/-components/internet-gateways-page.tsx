import type { InternetGateway } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, Plus, SlidersHorizontal, Trash2 } from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelTags,
} from "@/components/detail-panel"
import { PageHeading } from "@/components/page-heading"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useDeleteInternetGateway } from "@/mutations/ec2"

import { CreateInternetGatewayPage } from "./create-internet-gateway-page"

// A gateway with no attachment reports none; AWS's console calls that detached.
function attachmentState(igw: InternetGateway): string {
  return igw.Attachments?.[0]?.State ?? "detached"
}

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

function Overview({ igw }: { igw: InternetGateway }) {
  const vpcId = igw.Attachments?.[0]?.VpcId

  return (
    <PanelSection title="Details">
      <PanelFields>
        <PanelField copy={igw.InternetGatewayId} label="Gateway ID" mono>
          {igw.InternetGatewayId}
        </PanelField>
        <PanelField label="State">{attachmentState(igw)}</PanelField>
        <PanelField label="VPC" mono>
          {vpcId && (
            <Link
              className="text-primary hover:underline"
              params={{ id: vpcId }}
              to="/ec2/describe-vpcs/$id"
            >
              {vpcId}
            </Link>
          )}
        </PanelField>
        <PanelField label="Owner" mono>
          {igw.OwnerId}
        </PanelField>
      </PanelFields>
    </PanelSection>
  )
}

function Settings({
  igw,
  onDeleted,
}: {
  igw: InternetGateway
  onDeleted: () => void
}) {
  const id = igw.InternetGatewayId ?? ""
  const attached = !!igw.Attachments?.[0]?.VpcId
  const remove = useDeleteInternetGateway()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelTags tags={igw.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {attached && (
            <p className="text-sm text-muted-foreground">
              Detach it from its VPC on the gateway page before deleting it.
            </p>
          )}
          <Button
            disabled={attached || remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete internet gateway
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete internet gateway"
      />
    </>
  )
}

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
        <Suspense fallback={<Skeleton className="h-64" />}>
          <CreateInternetGatewayPage
            onClose={(igwId) => {
              onSearchChange({ ...CLOSED_PANEL, selected: igwId ?? "" })
            }}
          />
        </Suspense>
      </DetailPanel>
    )
  } else if (open?.InternetGatewayId) {
    const id = open.InternetGatewayId
    const name = getNameTag(open.Tags)

    panel = (
      <DetailPanel
        closeLabel="Close internet gateway details"
        fullPage={
          <Link
            aria-label="Open internet gateway page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            to="/ec2/describe-internet-gateways/$id"
          >
            <Maximize2 />
          </Link>
        }
        key={id}
        label="Internet gateway details"
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        subtitle={name ? id : undefined}
        summary={<StateBadge state={attachmentState(open)} />}
        tab={search.tab}
        tabs={[
          {
            id: "overview",
            label: "Overview",
            icon: Eye,
            content: <Overview igw={open} />,
          },
          {
            id: "settings",
            label: "Settings",
            icon: SlidersHorizontal,
            content: <Settings igw={open} onDeleted={closePanel} />,
          },
        ]}
        title={name ?? id}
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
