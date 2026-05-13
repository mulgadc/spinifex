import type { _Object } from "@aws-sdk/client-s3"
import { GetObjectCommand } from "@aws-sdk/client-s3"
import { Download, File, Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { getS3Client } from "@/lib/awsClient"
import { formatDateTime, formatSize } from "@/lib/utils"
import { useDeleteObject } from "@/mutations/s3"

interface ObjectListItemProps {
  object: _Object
  bucketName: string
  displayName: string
  fullKey: string
}

export function ObjectListItem({
  object,
  bucketName,
  displayName,
  fullKey,
}: ObjectListItemProps) {
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [downloadError, setDownloadError] = useState<Error | null>(null)
  const deleteMutation = useDeleteObject()

  async function downloadObject() {
    setDownloadError(null)
    try {
      const command = new GetObjectCommand({
        Bucket: bucketName,
        Key: fullKey,
      })
      const response = await getS3Client().send(command)

      if (response.Body) {
        // oxlint-disable-next-line typescript/no-unsafe-call, typescript/no-unsafe-member-access, typescript/no-unsafe-assignment
        const blob = await response.Body.transformToByteArray()
        // oxlint-disable-next-line typescript/no-unsafe-type-assertion
        const url = URL.createObjectURL(new Blob([blob as BlobPart]))

        const link = document.createElement("a")
        link.href = url
        link.download = displayName
        document.body.append(link)
        link.click()
        link.remove()
        URL.revokeObjectURL(url)
      }
    } catch (error) {
      setDownloadError(new Error("Failed to download object", { cause: error }))
    }
  }

  async function handleDelete() {
    try {
      await deleteMutation.mutateAsync({
        bucket: bucketName,
        key: fullKey,
      })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  return (
    <div key={object.Key}>
      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete object"
        />
      )}
      {downloadError && (
        <ErrorBanner error={downloadError} msg="Failed to download object" />
      )}
      <div className="flex items-center justify-between rounded-lg border bg-card p-4">
        <div className="flex items-center gap-3">
          <File className="size-5 text-muted-foreground" />
          <div>
            <h3 className="font-medium">{displayName}</h3>
            {object.LastModified && (
              <p className="text-sm text-muted-foreground">
                Last Modified: {formatDateTime(object.LastModified)}
              </p>
            )}
          </div>
        </div>
        <div className="flex items-center gap-3">
          <div className="text-sm text-muted-foreground">
            {formatSize(object.Size ?? 0)}
          </div>
          <button
            aria-label={`Download ${displayName}`}
            className="rounded-md p-2 transition-colors hover:bg-accent"
            onClick={downloadObject}
            type="button"
          >
            <Download className="size-4" />
          </button>
          <button
            aria-label={`Delete ${displayName}`}
            className="rounded-md p-2 text-destructive transition-colors hover:bg-accent"
            onClick={() => setShowDeleteDialog(true)}
            type="button"
          >
            <Trash2 className="size-4" />
          </button>
        </div>

        <DeleteConfirmationDialog
          description={
            <>
              Are you sure you want to delete <strong>{displayName}</strong>?
              This action cannot be undone.
            </>
          }
          isPending={deleteMutation.isPending}
          onConfirm={handleDelete}
          onOpenChange={setShowDeleteDialog}
          open={showDeleteDialog}
          title="Delete Object"
        />
      </div>
    </div>
  )
}
