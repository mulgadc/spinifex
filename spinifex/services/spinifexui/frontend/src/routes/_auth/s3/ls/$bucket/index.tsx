import type { _Object } from "@aws-sdk/client-s3"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { removeTrailingSlash } from "@/lib/utils"
import { useUploadObject } from "@/mutations/s3"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"
import { FolderListItem } from "@/routes/_auth/s3/-components/folder-list-item"
import { ObjectListItem } from "@/routes/_auth/s3/-components/object-list-item"
import { UploadButton } from "@/routes/_auth/s3/-components/upload-button"

export const Route = createFileRoute("/_auth/s3/ls/$bucket/")({
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...s3BucketObjectsQueryOptions(params.bucket),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.bucket} | S3 | Mulga`,
      },
    ],
  }),
  component: BucketObjects,
})

function BucketObjects() {
  const { bucket: bucketName } = Route.useParams()
  const { data } = useSuspenseQuery(s3BucketObjectsQueryOptions(bucketName))
  const uploadMutation = useUploadObject()

  const objects = data.Contents ?? []
  const commonPrefixes = data.CommonPrefixes ?? []

  return (
    <>
      <BackLink to="/s3/ls">Back to buckets</BackLink>
      <PageHeading
        actions={
          <UploadButton
            bucket={bucketName}
            isPending={uploadMutation.isPending}
            onUpload={uploadMutation.mutateAsync}
          />
        }
        title={bucketName}
      />
      {uploadMutation.error && (
        <ErrorBanner
          error={uploadMutation.error}
          msg="Failed to upload object"
        />
      )}
      {commonPrefixes.length > 0 && (
        <div className="mb-6">
          <h2 className="mb-3 text-lg font-semibold">Folders</h2>
          <div className="space-y-2">
            {commonPrefixes.map((prefix) => {
              if (!prefix.Prefix) {
                return null
              }

              const folderName = removeTrailingSlash(prefix.Prefix)

              return (
                <FolderListItem
                  bucketName={bucketName}
                  folderName={folderName}
                  fullPrefix={prefix.Prefix}
                  key={prefix.Prefix}
                />
              )
            })}
          </div>
        </div>
      )}
      {objects.length > 0 && (
        <div className="space-y-2">
          <h2 className="mb-3 text-lg font-semibold">Objects</h2>
          {objects.map((object: _Object) => {
            if (!object.Key) {
              return null
            }

            return (
              <ObjectListItem
                bucketName={bucketName}
                displayName={object.Key}
                fullKey={object.Key}
                key={object.Key}
                object={object}
              />
            )
          })}
        </div>
      )}
      {/* I don't want to display this if we are displaying folders still. This is fallback / error msg */}
      {objects.length === 0 && commonPrefixes.length === 0 && (
        <p className="text-muted-foreground">
          No objects found in this bucket.
        </p>
      )}
    </>
  )
}
