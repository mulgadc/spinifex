import type { _Object } from "@aws-sdk/client-s3"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import {
  buildFullS3Key,
  ensureTrailingSlash,
  extractDisplayName,
  removeTrailingSlash,
} from "@/lib/utils"
import { useUploadObject } from "@/mutations/s3"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"
import { FolderListItem } from "@/routes/_auth/s3/-components/folder-list-item"
import { ObjectListItem } from "@/routes/_auth/s3/-components/object-list-item"
import { UploadButton } from "@/routes/_auth/s3/-components/upload-button"

export const Route = createFileRoute("/_auth/s3/ls/$bucket/$")({
  loader: async ({ context, params }) => {
    const prefix = ensureTrailingSlash(params._splat ?? "")
    await context.queryClient.ensureQueryData(
      s3BucketObjectsQueryOptions(params.bucket, prefix),
    )
  },
  head: ({ params }) => {
    const path = buildFullS3Key(params._splat ?? "", `${params.bucket}/`)
    return {
      meta: [
        {
          title: `${path} | S3 | Mulga`,
        },
      ],
    }
  },
  component: BucketObjectsWithPrefix,
})

function BucketObjectsWithPrefix() {
  const { bucket, _splat } = Route.useParams()

  const prefix = ensureTrailingSlash(_splat ?? "")

  const { data } = useSuspenseQuery(s3BucketObjectsQueryOptions(bucket, prefix))
  const uploadMutation = useUploadObject()

  const objects = data.Contents ?? []
  const commonPrefixes = data.CommonPrefixes ?? []

  // Build breadcrumb navigation
  const pathParts = prefix.split("/").filter(Boolean)
  const breadcrumbs = [
    { name: bucket, path: `/s3/ls/${bucket}` },
    ...pathParts.map((part, index) => {
      const path = `${pathParts.slice(0, index + 1).join("/")}/`
      return {
        name: part,
        path: `/s3/ls/${bucket}/${path}`,
      }
    }),
  ]

  // Get parent path for back navigation
  const parentSplat = `${pathParts.slice(0, -1).join("/")}/`

  return (
    <>
      {pathParts.length > 1 ? (
        <BackLink
          params={{ bucket, _splat: parentSplat }}
          to="/s3/ls/$bucket/$"
        >
          Back
        </BackLink>
      ) : (
        <BackLink params={{ bucket }} to="/s3/ls/$bucket">
          Back
        </BackLink>
      )}
      <div className="mb-6">
        <div className="mb-2 flex items-center justify-between">
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            {breadcrumbs.map((crumb, index) => (
              <div className="flex items-center gap-2" key={crumb.path}>
                {index > 0 && <span>/</span>}
                {index === breadcrumbs.length - 1 ? (
                  <span className="text-foreground">{crumb.name}</span>
                ) : (
                  <Link className="hover:text-foreground" to={crumb.path}>
                    {crumb.name}
                  </Link>
                )}
              </div>
            ))}
          </div>
          <UploadButton
            bucket={bucket}
            isPending={uploadMutation.isPending}
            onUpload={uploadMutation.mutateAsync}
            prefix={prefix}
          />
        </div>
      </div>
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
            {commonPrefixes.map((prefixObj) => {
              if (!prefixObj.Prefix) {
                return null
              }

              const folderName = removeTrailingSlash(
                extractDisplayName(prefixObj.Prefix, prefix),
              )
              const fullPrefix = buildFullS3Key(prefixObj.Prefix, prefix)

              return (
                <FolderListItem
                  bucketName={bucket}
                  folderName={folderName}
                  fullPrefix={fullPrefix}
                  key={prefixObj.Prefix}
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

            const displayName = extractDisplayName(object.Key, prefix)
            const fullKey = buildFullS3Key(object.Key, prefix)

            return (
              <ObjectListItem
                bucketName={bucket}
                displayName={displayName}
                fullKey={fullKey}
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
          No objects found in this folder.
        </p>
      )}
    </>
  )
}
