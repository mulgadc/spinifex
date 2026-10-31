import type { LaunchTemplate } from "@aws-sdk/client-ec2"
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
import { buttonVariants } from "@/components/ui/button"
import { useRowSelection } from "@/hooks/use-row-selection"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteLaunchTemplate } from "@/mutations/ec2"

import { CreateLaunchTemplatePage } from "./create-launch-template-page"
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

  const selection = useRowSelection(
    launchTemplates.map((lt) => lt.LaunchTemplateId ?? ""),
    open !== undefined,
    () => {
      onSearchChange(CLOSED_PANEL)
    },
  )

  const deleteLaunchTemplate = useDeleteLaunchTemplate()

  const names = new Map(
    launchTemplates.map((lt) => [lt.LaunchTemplateId, lt.LaunchTemplateName]),
  )

  let panel: ReactNode = null

  if (search.create) {
    panel = (
      <CreatePanel
        onClose={() => {
          onSearchChange(CLOSED_PANEL)
        }}
        title="Create launch template"
      >
        <CreateLaunchTemplatePage
          onClose={(id) => {
            onSearchChange({ ...CLOSED_PANEL, selected: id ?? "" })
          }}
        />
      </CreatePanel>
    )
  } else if (open?.LaunchTemplateId) {
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
          <Link
            className={buttonVariants()}
            from="/ec2/describe-launch-templates/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create launch template
          </Link>
        }
        title="Launch templates"
      />
      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            deleteOne={deleteLaunchTemplate.mutateAsync}
            description="All of their versions will be deleted. This action cannot be undone."
            ids={selection.ids}
            label={(id) => {
              const name = names.get(id)

              return name ? `${name} (${id})` : id
            }}
            noun={["launch template", "launch templates"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}
      <DataTable
        activeId={open?.LaunchTemplateId}
        caption="Launch templates"
        columns={COLUMNS}
        empty="No launch templates found."
        getRowId={(lt) => lt.LaunchTemplateId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={launchTemplates}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
