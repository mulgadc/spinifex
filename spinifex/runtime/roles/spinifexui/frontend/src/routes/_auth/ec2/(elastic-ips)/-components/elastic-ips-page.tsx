import type { Address } from "@aws-sdk/client-ec2"
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

import { AllocateAddressPage } from "./allocate-address-page"
import { associationState, ElasticIpDetails } from "./elastic-ip-details"

const COLUMNS: DataColumn<Address>[] = [
  {
    id: "address",
    header: "Elastic IP",
    sortValue: (address) => getNameTag(address.Tags) ?? address.PublicIp,
    cell: (address) => {
      const name = getNameTag(address.Tags)
      const id = address.AllocationId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-addresses/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {name ?? address.PublicIp}
          </span>
          {name && (
            <span className="block truncate font-mono text-xs text-muted-foreground">
              {address.PublicIp}
            </span>
          )}
        </Link>
      )
    },
  },
  {
    id: "state",
    header: "State",
    sortValue: associationState,
    cell: (address) => <StateBadge state={associationState(address)} />,
  },
  {
    id: "allocation",
    header: "Allocation ID",
    sortValue: (address) => address.AllocationId,
    hideBelow: "md",
    cell: (address) => (
      <span className="font-mono text-xs">{address.AllocationId}</span>
    ),
  },
  {
    id: "instance",
    header: "Instance",
    sortValue: (address) => address.InstanceId,
    hideBelow: "sm",
    cell: (address) =>
      address.InstanceId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: address.InstanceId }}
          to="/ec2/describe-instances/$id"
        >
          {address.InstanceId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "private",
    header: "Private IP",
    sortValue: (address) => address.PrivateIpAddress,
    hideBelow: "lg",
    cell: (address) => (
      <span className="font-mono text-xs">
        {address.PrivateIpAddress ?? "—"}
      </span>
    ),
  },
]

interface ElasticIpsPageProps {
  addresses: readonly Address[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function ElasticIpsPage({
  addresses,
  search,
  onSearchChange,
}: ElasticIpsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = addresses.find(
    (address) =>
      search.selected !== "" && address.AllocationId === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <DetailPanel
        closeLabel="Close allocate form"
        label="Allocate Elastic IP"
        onClose={closePanel}
        title="Allocate Elastic IP"
      >
        <Suspense fallback={<PanelSpinner />}>
          <AllocateAddressPage
            onClose={(allocationId) => {
              onSearchChange({ ...CLOSED_PANEL, selected: allocationId ?? "" })
            }}
          />
        </Suspense>
      </DetailPanel>
    )
  } else if (open?.AllocationId) {
    panel = (
      <ElasticIpDetails
        address={open}
        key={open.AllocationId}
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
            from="/ec2/describe-addresses/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Allocate Elastic IP
          </Link>
        }
        subtitle="Static public addresses you can move between instances."
        title="Elastic IPs"
      />
      <DataTable
        activeId={open?.AllocationId}
        caption="Elastic IPs"
        columns={COLUMNS}
        empty="No Elastic IPs yet."
        getRowId={(address) => address.AllocationId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={addresses}
        sort={sort}
      />
    </ResourceView>
  )
}
