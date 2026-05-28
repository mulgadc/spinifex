import type { Bucket } from "@aws-sdk/client-s3"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { s3BucketsQueryOptions } from "@/queries/s3"

export const Route = createFileRoute("/_auth/s3/ls/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(s3BucketsQueryOptions)
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

      {buckets.length > 0 ? (
        <div className="space-y-4">
          {buckets.map((bucket: Bucket) => {
            if (!bucket.Name) {
              return null
            }
            return (
              <ListCard
                key={bucket.Name}
                params={{ bucket: bucket.Name }}
                subtitle={
                  bucket.CreationDate
                    ? `Created: ${formatDateTime(bucket.CreationDate)}`
                    : undefined
                }
                title={bucket.Name}
                to="/s3/ls/$bucket"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No buckets found.</p>
      )}
    </>
  )
}
