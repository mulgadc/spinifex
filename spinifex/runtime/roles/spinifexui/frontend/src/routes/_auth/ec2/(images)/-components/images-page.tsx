import type { Image } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { type ReactNode, useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { StateBadge } from "@/components/state-badge"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"

import { ImageDetails } from "./image-details"

const COLUMNS: DataColumn<Image>[] = [
  {
    id: "image",
    header: "Image",
    sortValue: (image) => image.Name ?? image.ImageId,
    cell: (image) => {
      const id = image.ImageId ?? ""

      return (
        <Link
          className="group block max-w-72 min-w-0 rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          from="/ec2/describe-images/"
          search={(previous) => ({
            ...previous,
            ...CLOSED_PANEL,
            selected: id,
          })}
          to="."
        >
          <span className="block truncate font-medium text-primary group-hover:underline">
            {image.Name ?? id}
          </span>
          {image.Name && (
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
    sortValue: (image) => image.State,
    cell: (image) => <StateBadge state={image.State} />,
  },
  {
    id: "architecture",
    header: "Architecture",
    sortValue: (image) => image.Architecture,
    hideBelow: "sm",
    cell: (image) => image.Architecture,
  },
  {
    id: "owner",
    header: "Owner",
    sortValue: (image) => image.OwnerId,
    hideBelow: "lg",
    cell: (image) => (
      <span className="font-mono text-xs tabular-nums">{image.OwnerId}</span>
    ),
  },
  {
    id: "created",
    header: "Created",
    sortValue: (image) => image.CreationDate,
    hideBelow: "xl",
    cell: (image) => formatDateTime(image.CreationDate),
  },
]

interface ImagesPageProps {
  images: readonly Image[]
  search: PanelSearch
  onSearchChange: (search: Partial<PanelSearch>) => void
}

export function ImagesPage({
  images,
  search,
  onSearchChange,
}: ImagesPageProps) {
  const [sort, setSort] = useState<SortState>()

  const open = images.find(
    (image) => search.selected !== "" && image.ImageId === search.selected,
  )

  let panel: ReactNode = null

  if (open?.ImageId) {
    panel = (
      <ImageDetails
        image={open}
        key={open.ImageId}
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
      <PageHeading title="Images" />
      <DataTable
        activeId={open?.ImageId}
        caption="Images"
        columns={COLUMNS}
        empty="No images found."
        getRowId={(image) => image.ImageId ?? ""}
        onRowClick={(id) => {
          onSearchChange({ ...CLOSED_PANEL, selected: id })
        }}
        onSortChange={setSort}
        rows={images}
        sort={sort}
      />
    </ResourceView>
  )
}
