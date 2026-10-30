import { useQuery } from "@tanstack/react-query"
import { Download, Eye, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Button } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime, formatSize } from "@/lib/utils"
import { downloadObject, useDeleteObject } from "@/mutations/s3"
import { s3ObjectHeadQueryOptions } from "@/queries/s3"

import { storageClassLabel } from "./object-labels"

interface ObjectDetailsProps {
  bucket: string
  objectKey: string
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
}

function useDownload(bucket: string, objectKey: string) {
  const [error, setError] = useState<Error | null>(null)
  const [pending, setPending] = useState(false)

  async function download() {
    setError(null)
    setPending(true)

    try {
      await downloadObject(bucket, objectKey)
    } catch (error_) {
      setError(new Error("Failed to download object", { cause: error_ }))
    } finally {
      setPending(false)
    }
  }

  return { download, error, pending }
}

// One object's properties and metadata, read with HeadObject, beside the
// bucket's listing.
export function ObjectDetails({
  bucket,
  objectKey,
  tab,
  onTabChange,
  onClose,
}: ObjectDetailsProps) {
  const head = useQuery(s3ObjectHeadQueryOptions(bucket, objectKey))
  const { download, error, pending } = useDownload(bucket, objectKey)
  const name = objectKey.split("/").at(-1) || objectKey

  let overview = <PanelSpinner />

  if (head.error) {
    overview = <ErrorBanner error={head.error} msg="Failed to read object" />
  } else if (head.data) {
    const { data } = head
    const metadata = Object.entries(data.Metadata ?? {})

    overview = (
      <>
        <PanelSection title="Properties">
          <PanelFields>
            <PanelField copy={objectKey} label="Key" mono>
              <span title={objectKey}>{objectKey}</span>
            </PanelField>
            <PanelField
              copy={`s3://${bucket}/${objectKey}`}
              label="S3 URI"
              mono
            >
              s3://{bucket}/{objectKey}
            </PanelField>
            <PanelField
              copy={`arn:aws:s3:::${bucket}/${objectKey}`}
              label="ARN"
              mono
            >
              arn:aws:s3:::{bucket}/{objectKey}
            </PanelField>
            <PanelField label="Size">
              {data.ContentLength === undefined
                ? undefined
                : `${formatSize(data.ContentLength)} (${data.ContentLength.toLocaleString()} bytes)`}
            </PanelField>
            <PanelField label="Last modified">
              {data.LastModified && formatDateTime(data.LastModified)}
            </PanelField>
            <PanelField label="Type">{data.ContentType}</PanelField>
            <PanelField copy={data.ETag?.replaceAll('"', "")} label="ETag" mono>
              {data.ETag?.replaceAll('"', "")}
            </PanelField>
            {/* HeadObject names the class only when it is not Standard. */}
            <PanelField label="Storage class">
              {storageClassLabel(data.StorageClass ?? "STANDARD")}
            </PanelField>
            {data.VersionId && (
              <PanelField copy={data.VersionId} label="Version ID" mono>
                {data.VersionId}
              </PanelField>
            )}
            {data.ServerSideEncryption && (
              <PanelField label="Encryption">
                {data.ServerSideEncryption}
              </PanelField>
            )}
          </PanelFields>
        </PanelSection>
        <PanelSection title="Metadata">
          <PanelFields>
            <PanelField label="Content-Type" mono>
              {data.ContentType}
            </PanelField>
            {data.CacheControl && (
              <PanelField label="Cache-Control" mono>
                {data.CacheControl}
              </PanelField>
            )}
            {data.ContentEncoding && (
              <PanelField label="Content-Encoding" mono>
                {data.ContentEncoding}
              </PanelField>
            )}
            {data.ContentDisposition && (
              <PanelField label="Content-Disposition" mono>
                {data.ContentDisposition}
              </PanelField>
            )}
            {metadata.map(([key, value]) => (
              <PanelField
                copy={value}
                key={key}
                label={`x-amz-meta-${key}`}
                mono
              >
                {value}
              </PanelField>
            ))}
          </PanelFields>
          {metadata.length === 0 && (
            <p className="mt-2 text-xs text-muted-foreground">
              No user-defined metadata.
            </p>
          )}
        </PanelSection>
      </>
    )
  }

  return (
    <DetailPanel
      closeLabel="Close object details"
      label="Object details"
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={objectKey === name ? undefined : objectKey}
      summary={
        <>
          <Button
            disabled={pending}
            onClick={() => {
              void download()
            }}
            size="sm"
            variant="outline"
          >
            <Download data-icon="inline-start" />
            {pending ? "Downloading…" : "Download"}
          </Button>
          {error && (
            <ErrorBanner error={error} msg="Failed to download object" />
          )}
        </>
      }
      tab={tab}
      tabs={[
        { id: "overview", label: "Overview", icon: Eye, content: overview },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: (
            <Settings
              bucket={bucket}
              objectKey={objectKey}
              onDeleted={onClose}
            />
          ),
        },
      ]}
      title={name}
    />
  )
}

function Settings({
  bucket,
  objectKey,
  onDeleted,
}: {
  bucket: string
  objectKey: string
  onDeleted: () => void
}) {
  const remove = useDeleteObject()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete object
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        confirmPhrase="delete"
        description={`Delete ${objectKey} from ${bucket}? This cannot be undone.`}
        identifier={{ bucket, key: objectKey }}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete object"
      />
    </>
  )
}
