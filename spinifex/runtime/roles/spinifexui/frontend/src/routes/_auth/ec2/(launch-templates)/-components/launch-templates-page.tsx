import type { LaunchTemplate } from "@aws-sdk/client-ec2"
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
import { formatDateTime } from "@/lib/utils"

import { LaunchTemplateDetails } from "./launch-template-details"

const COLUMNS: DataColumn<LaunchTemplate>[] = [
  {
    id: "template",
    header: "Launch template",
    sortValue: (lt) => lt.LaunchTemplateName ?? lt.LaunchTemplateId,
    cell: (lt) => {
      const name = lt.LaunchTemplateName
      const id = lt.LaunchTemplateId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-launch-templates/"
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
    id: "default",
    header: "Default version",
    sortValue: (lt) => lt.DefaultVersionNumber,
    cell: (lt) => (
      <span className="tabular-nums">{lt.DefaultVersionNumber}</span>
    ),
  },
  {
    id: "latest",
    header: "Latest version",
    sortValue: (lt) => lt.LatestVersionNumber,
    hideBelow: "sm",
    cell: (lt) => (
      <span className="tabular-nums">{lt.LatestVersionNumber}</span>
    ),
  },
  {
    id: "created",
    header: "Created",
    sortValue: (lt) => lt.CreateTime?.getTime(),
    hideBelow: "lg",
    cell: (lt) => formatDateTime(lt.CreateTime),
  },
]

interface LaunchTemplatesPageProps {
  launchTemplates: readonly LaunchTemplate[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function LaunchTemplatesPage({
  launchTemplates,
  search,
  onSearchChange,
}: LaunchTemplatesPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = launchTemplates.find(
    (lt) => search.selected !== "" && lt.LaunchTemplateId === search.selected,
  )

  let panel: ReactNode = null

  if (open?.LaunchTemplateId) {
    panel = (
      <LaunchTemplateDetails
        key={open.LaunchTemplateId}
        launchTemplate={open}
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
          <Link className={buttonVariants()} to="/ec2/create-launch-template">
            <Plus data-icon="inline-start" />
            Create launch template
          </Link>
        }
        title="Launch templates"
      />
      <DataTable
        activeId={open?.LaunchTemplateId}
        caption="Launch templates"
        columns={COLUMNS}
        empty="No launch templates found."
        getRowId={(lt) => lt.LaunchTemplateId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={launchTemplates}
        sort={sort}
      />
    </ResourceView>
  )
}
