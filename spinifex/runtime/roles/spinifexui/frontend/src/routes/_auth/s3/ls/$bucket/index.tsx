import { createFileRoute } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { useUploadObject } from "@/mutations/s3"
import { s3BucketObjectsQueryOptions } from "@/queries/s3"
import { BucketObjectList } from "@/routes/_auth/s3/-components/bucket-object-list"
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
  const uploadMutation = useUploadObject()

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
      <BucketObjectList bucket={bucketName} prefix="" />
    </>
  )
}
