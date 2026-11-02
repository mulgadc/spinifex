import type { Bucket } from "@aws-sdk/client-s3"
import { useSuspenseQuery } from "@tanstack/react-query"
import {
  createFileRoute,
  Link,
  stripSearchParams,
} from "@tanstack/react-router"
import { Database, Plus } from "lucide-react"
import { useState } from "react"
import { z } from "zod"

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
import { CLOSED_PANEL, panelSearchFields } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteBucket } from "@/mutations/s3"
import { s3BucketsQueryOptions } from "@/queries/s3"

import { CreateBucketPage } from "../(buckets)/-components/create-bucket-page"

export const Route = createFileRoute("/_auth/s3/ls/")({
  validateSearch: z.object(panelSearchFields),
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...s3BucketsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Buckets | Object Storage | Mulga",
      },
    ],
  }),
  component: Buckets,
})

// A bucket opens its object browser rather than a side panel: its contents
// are the page, and they need the full width.
const COLUMNS: DataColumn<Bucket>[] = [
  {
    id: "name",
    header: "Name",
    sortValue: (bucket) => bucket.Name,
    cell: (bucket) => (
      <Link
        className="inline-flex max-w-96 min-w-0 items-center gap-2 rounded-sm font-medium text-primary hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        params={{ bucket: bucket.Name ?? "" }}
        to="/s3/ls/$bucket"
      >
        <Database aria-hidden className="size-4 shrink-0" />
        <span className="truncate">{bucket.Name}</span>
      </Link>
    ),
  },
  {
    id: "created",
    header: "Created",
    sortValue: (bucket) => bucket.CreationDate?.getTime(),
    cell: (bucket) =>
      bucket.CreationDate ? formatDateTime(bucket.CreationDate) : "—",
  },
]

function noPanel() {
  // Buckets open as pages, so only the create form uses the panel.
}

function Buckets() {
  const { data } = useSuspenseQuery(s3BucketsQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  const [sort, setSort] = useState<SortState>()
  const deleteBucket = useDeleteBucket()

  const buckets = (data.Buckets ?? []).filter((bucket) => !!bucket.Name)

  const selection = useRowSelection(
    buckets.map((bucket) => bucket.Name ?? ""),
    false,
    noPanel,
  )

  // Opening or closing the panel adds a history entry, so Back closes it.
  function closePanel() {
    void navigate({ search: (previous) => ({ ...previous, ...CLOSED_PANEL }) })
  }

  const panel = search.create && (
    <CreatePanel onClose={closePanel} title="Create bucket">
      <CreateBucketPage onClose={closePanel} />
    </CreatePanel>
  )

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/s3/ls/"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Create bucket
          </Link>
        }
        title="Buckets"
      />

      {selection.ids.length > 0 && (
        <SelectionBar count={selection.ids.length} onClear={selection.clear}>
          <BulkDelete
            confirmPhrase="delete"
            deleteOne={deleteBucket.mutateAsync}
            description="Only an empty bucket can be deleted. One that still holds objects is refused and left as it is, so empty it from its object browser first."
            ids={selection.ids}
            noun={["bucket", "buckets"]}
            onDeleted={selection.drop}
          />
        </SelectionBar>
      )}

      <DataTable
        caption="Buckets"
        columns={COLUMNS}
        empty="No buckets yet."
        getRowId={(bucket) => bucket.Name ?? ""}
        onRowClick={(bucket) => {
          void navigate({ to: "/s3/ls/$bucket", params: { bucket } })
        }}
        onSelectedChange={selection.onSelectedChange}
        onSortChange={setSort}
        rows={buckets}
        selected={selection.selected}
        sort={sort}
      />
    </ResourceView>
  )
}
