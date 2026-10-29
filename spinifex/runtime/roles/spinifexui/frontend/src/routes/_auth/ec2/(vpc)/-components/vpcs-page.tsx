import type { Vpc } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
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
import { useDeleteVpc } from "@/mutations/ec2"
import { ec2SubnetsQueryOptions } from "@/queries/ec2"

import { CreateVpcPage } from "./create-vpc-page"

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

function Overview({ vpc }: { vpc: Vpc }) {
  const { data } = useQuery(ec2SubnetsQueryOptions)
  const subnets = data?.Subnets?.filter((s) => s.VpcId === vpc.VpcId) ?? []

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={vpc.VpcId} label="VPC ID" mono>
            {vpc.VpcId}
          </PanelField>
          <PanelField copy={vpc.CidrBlock} label="CIDR" mono>
            {vpc.CidrBlock}
          </PanelField>
          <PanelField label="Default">
            {vpc.IsDefault ? "Yes" : "No"}
          </PanelField>
          <PanelField label="Tenancy">{vpc.InstanceTenancy}</PanelField>
          <PanelField label="DHCP options" mono>
            {vpc.DhcpOptionsId}
          </PanelField>
          <PanelField label="Owner" mono>
            {vpc.OwnerId}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title={`Subnets (${subnets.length})`}>
        {subnets.length === 0 ? (
          <p className="text-sm text-muted-foreground">No subnets.</p>
        ) : (
          <PanelFields>
            {subnets.map((subnet) => (
              <PanelField
                key={subnet.SubnetId}
                label={subnet.AvailabilityZone ?? "—"}
                mono
              >
                <Link
                  className="text-primary hover:underline"
                  params={{ id: subnet.SubnetId ?? "" }}
                  to="/ec2/describe-subnets/$id"
                >
                  {subnet.CidrBlock}
                </Link>
              </PanelField>
            ))}
          </PanelFields>
        )}
      </PanelSection>
    </>
  )
}

function Settings({ vpc, onDeleted }: { vpc: Vpc; onDeleted: () => void }) {
  const id = vpc.VpcId ?? ""
  const remove = useDeleteVpc()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelTags tags={vpc.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete VPC
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? It must have no subnets or internet gateways left. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete VPC"
      />
    </>
  )
}

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
    const id = open.VpcId
    const name = getNameTag(open.Tags)

    panel = (
      <DetailPanel
        closeLabel="Close VPC details"
        fullPage={
          <Link
            aria-label="Open VPC page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            to="/ec2/describe-vpcs/$id"
          >
            <Maximize2 />
          </Link>
        }
        key={id}
        label="VPC details"
        onClose={closePanel}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        subtitle={name ? id : undefined}
        summary={<StateBadge state={open.State} />}
        tab={search.tab}
        tabs={[
          {
            id: "overview",
            label: "Overview",
            icon: Eye,
            content: <Overview vpc={open} />,
          },
          {
            id: "settings",
            label: "Settings",
            icon: SlidersHorizontal,
            content: <Settings onDeleted={closePanel} vpc={open} />,
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
