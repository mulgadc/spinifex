import type { SecurityGroup } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { type ReactNode, useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"

import { SecurityGroupDetails } from "./security-group-details"

// Rules count one per source, as AWS lists and revokes them.
function ruleCount(rules: SecurityGroup["IpPermissions"]): number {
  return (rules ?? []).reduce(
    (total, rule) =>
      total +
      (rule.IpRanges?.length ?? 0) +
      (rule.Ipv6Ranges?.length ?? 0) +
      (rule.UserIdGroupPairs?.length ?? 0) +
      (rule.PrefixListIds?.length ?? 0),
    0,
  )
}

const COLUMNS: DataColumn<SecurityGroup>[] = [
  {
    id: "group",
    header: "Security group",
    sortValue: (sg) => sg.GroupName ?? sg.GroupId,
    cell: (sg) => {
      const id = sg.GroupId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-security-groups/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {sg.GroupName ?? id}
          </span>
          {sg.GroupName && (
            <span className="block truncate font-mono text-xs text-muted-foreground">
              {id}
            </span>
          )}
        </Link>
      )
    },
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (sg) => sg.VpcId,
    hideBelow: "sm",
    cell: (sg) =>
      sg.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: sg.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {sg.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "description",
    header: "Description",
    sortValue: (sg) => sg.Description,
    hideBelow: "lg",
    cell: (sg) => (
      <span className="block max-w-80 truncate" title={sg.Description}>
        {sg.Description}
      </span>
    ),
  },
  {
    id: "inbound",
    header: "Inbound",
    sortValue: (sg) => ruleCount(sg.IpPermissions),
    hideBelow: "md",
    align: "end",
    cell: (sg) => (
      <span className="tabular-nums">{ruleCount(sg.IpPermissions)}</span>
    ),
  },
  {
    id: "outbound",
    header: "Outbound",
    sortValue: (sg) => ruleCount(sg.IpPermissionsEgress),
    hideBelow: "md",
    align: "end",
    cell: (sg) => (
      <span className="tabular-nums">{ruleCount(sg.IpPermissionsEgress)}</span>
    ),
  },
]

interface SecurityGroupsPageProps {
  securityGroups: readonly SecurityGroup[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function SecurityGroupsPage({
  securityGroups,
  search,
  onSearchChange,
}: SecurityGroupsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = securityGroups.find(
    (sg) => search.selected !== "" && sg.GroupId === search.selected,
  )

  let panel: ReactNode = null

  if (open?.GroupId) {
    panel = (
      <SecurityGroupDetails
        key={open.GroupId}
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        onTabChange={(tab) => {
          onSearchChange({ tab })
        }}
        sg={open}
        tab={search.tab}
      />
    )
  }

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/ec2/create-security-group">
            <Plus data-icon="inline-start" />
            Create security group
          </Link>
        }
        subtitle="Stateful firewalls for the instances in a VPC."
        title="Security groups"
      />
      <DataTable
        activeId={open?.GroupId}
        caption="Security groups"
        columns={COLUMNS}
        empty="No security groups yet."
        getRowId={(sg) => sg.GroupId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={securityGroups}
        sort={sort}
      />
    </ResourceView>
  )
}
