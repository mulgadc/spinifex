import type { Bucket } from "@aws-sdk/client-s3"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { type DataColumn, DataTable } from "@/components/data-table"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { s3BucketsQueryOptions } from "@/queries/s3"

export const Route = createFileRoute("/_auth/s3/ls/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...s3BucketsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Buckets | S3 | Mulga",
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
        className="font-medium text-primary hover:underline"
        params={{ bucket: bucket.Name ?? "" }}
        to="/s3/ls/$bucket"
      >
        {bucket.Name}
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

function Buckets() {
  const { data } = useSuspenseQuery(s3BucketsQueryOptions)

  const buckets = data.Buckets ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/s3/create-bucket">
            <Button>Create Bucket</Button>
          </Link>
        }
        title="Buckets"
      />

      <DataTable
        caption="Buckets"
        columns={COLUMNS}
        empty="No buckets found."
        getRowId={(bucket) => bucket.Name ?? ""}
        rows={buckets}
      />
    </>
  )
}
