import { createFileRoute, Link } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { buildFullS3Key, ensureTrailingSlash } from "@/lib/utils"
import { useUploadObject } from "@/mutations/s3"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"
import { BucketObjectList } from "@/routes/_auth/s3/-components/bucket-object-list"
import { UploadButton } from "@/routes/_auth/s3/-components/upload-button"

export const Route = createFileRoute("/_auth/s3/ls/$bucket/$")({
  loader: async ({ context, params }) => {
    const prefix = ensureTrailingSlash(params._splat ?? "")
    await context.queryClient.query({
      ...s3BucketObjectsQueryOptions(params.bucket, prefix),
      staleTime: "static",
    })
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

  const uploadMutation = useUploadObject()

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
      <BucketObjectList bucket={bucket} prefix={prefix} />
    </>
  )
}
