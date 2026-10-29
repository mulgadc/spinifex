import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { elbv2TargetHealthQueryOptions } from "@/queries/elbv2"

import { TargetGroupDetails } from "./target-group-details"

// Both health columns read the same query, so it is fetched once per group.
function TargetHealthCell({
  arn,
  show,
}: {
  arn: string
  show: "count" | "summary"
}) {
  const { data, isLoading } = useQuery(elbv2TargetHealthQueryOptions(arn))

  if (isLoading) {
    return <span className="text-muted-foreground">…</span>
  }

  const descriptions = data?.TargetHealthDescriptions ?? []

  if (show === "count") {
    return descriptions.length
  }

  const counts: Record<string, number> = {}

  for (const desc of descriptions) {
    const state = desc.TargetHealth?.State ?? "unknown"
    counts[state] = (counts[state] ?? 0) + 1
  }

  const summary = Object.entries(counts)
    .map(([state, count]) => `${count} ${state}`)
    .join(", ")

  return summary ? (
    <span className="text-xs">{summary}</span>
  ) : (
    <span className="text-muted-foreground">—</span>
  )
}

const COLUMNS: DataColumn<TargetGroup>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (tg) => tg.TargetGroupName,
    cell: (tg) => (
      <Link
        className="block max-w-72 truncate rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        from="/ec2/describe-target-groups/"
        search={(previous) => ({
          ...previous,
          ...CLOSED_PANEL,
          selected: tg.TargetGroupArn ?? "",
        })}
        to="."
      >
        {tg.TargetGroupName ?? tg.TargetGroupArn}
      </Link>
    ),
  },
  {
    id: "protocol",
    header: "Protocol",
    sortValue: (tg) => tg.Protocol,
    hideBelow: "sm",
    cell: (tg) => tg.Protocol,
  },
  {
    id: "port",
    header: "Port",
    sortValue: (tg) => tg.Port,
    hideBelow: "sm",
    cell: (tg) => tg.Port,
  },
  {
    id: "vpc",
    header: "VPC",
    sortValue: (tg) => tg.VpcId,
    hideBelow: "lg",
    cell: (tg) =>
      tg.VpcId ? (
        <Link
          className="font-mono text-xs text-primary hover:underline"
          params={{ id: tg.VpcId }}
          to="/ec2/describe-vpcs/$id"
        >
          {tg.VpcId}
        </Link>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    id: "type",
    header: "Target type",
    sortValue: (tg) => tg.TargetType,
    hideBelow: "md",
    cell: (tg) => tg.TargetType,
  },
  {
    id: "targets",
    header: "Targets",
    hideBelow: "md",
    cell: (tg) => (
      <TargetHealthCell arn={tg.TargetGroupArn ?? ""} show="count" />
    ),
  },
  {
    id: "health",
    header: "Health",
    cell: (tg) => (
      <TargetHealthCell arn={tg.TargetGroupArn ?? ""} show="summary" />
    ),
  },
]

interface DescribeTargetGroupsPageProps {
  targetGroups: readonly TargetGroup[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function DescribeTargetGroupsPage({
  targetGroups,
  search,
  onSearchChange,
}: DescribeTargetGroupsPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = targetGroups.find(
    (tg) => search.selected !== "" && tg.TargetGroupArn === search.selected,
  )

  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  return (
    <ResourceView
      panel={
        open?.TargetGroupArn && (
          <TargetGroupDetails
            key={open.TargetGroupArn}
            onClose={closePanel}
            onTabChange={(tab) => {
              onSearchChange({ tab })
            }}
            tab={search.tab}
            tg={open}
          />
        )
      }
    >
      <PageHeading
        actions={
          <Link className={buttonVariants()} to="/ec2/create-target-group">
            <Plus data-icon="inline-start" />
            Create target group
          </Link>
        }
        subtitle="The instances a load balancer forwards to."
        title="Target groups"
      />
      <DataTable
        activeId={open?.TargetGroupArn}
        caption="Target groups"
        columns={COLUMNS}
        empty="No target groups yet."
        getRowId={(tg) => tg.TargetGroupArn ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={targetGroups}
        sort={sort}
      />
    </ResourceView>
  )
}
