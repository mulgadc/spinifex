import type {
  DataSourceSummary,
  IngestionJobSummary,
} from "@aws-sdk/client-bedrock-agent"
import { useQuery } from "@tanstack/react-query"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { useDeleteDataSource } from "@/mutations/bedrockAgent"
import { ingestionJobsQueryOptions } from "@/queries/bedrockAgent"

const RECENT_JOBS_SHOWN = 3

function IngestionJobRow({ job }: { job: IngestionJobSummary }) {
  const stats = job.statistics
  return (
    <div className="rounded-md border p-2 text-xs" key={job.ingestionJobId}>
      <div className="mb-1 flex items-center justify-between gap-2">
        <span className="font-mono">{job.ingestionJobId}</span>
        <StateBadge state={job.status} />
      </div>
      <div className="text-muted-foreground">
        Started {formatDateTime(job.startedAt)}
      </div>
      {stats && (
        <div className="mt-1 text-muted-foreground">
          scanned {stats.numberOfDocumentsScanned ?? 0} · indexed{" "}
          {(stats.numberOfNewDocumentsIndexed ?? 0) +
            (stats.numberOfModifiedDocumentsIndexed ?? 0)}{" "}
          · failed {stats.numberOfDocumentsFailed ?? 0}
        </div>
      )}
    </div>
  )
}

interface DataSourceCardProps {
  knowledgeBaseId: string
  dataSource: DataSourceSummary
}

export function DataSourceCard({
  knowledgeBaseId,
  dataSource,
}: DataSourceCardProps) {
  const dataSourceId = dataSource.dataSourceId ?? ""
  const [showDelete, setShowDelete] = useState(false)
  const { data, isLoading, isError, error } = useQuery(
    ingestionJobsQueryOptions(knowledgeBaseId, dataSourceId),
  )
  const deleteDataSource = useDeleteDataSource()

  const jobs = (data?.ingestionJobSummaries ?? []).slice(0, RECENT_JOBS_SHOWN)

  async function handleDelete() {
    try {
      await deleteDataSource.mutateAsync({ dataSourceId, knowledgeBaseId })
      setShowDelete(false)
    } catch {
      // Left open so the refusal in the dialog description stays readable.
    }
  }

  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="mb-2 flex items-center justify-between">
        <div>
          <h3 className="font-medium">{dataSource.name}</h3>
          <p className="font-mono text-xs text-muted-foreground">
            {dataSourceId}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <StateBadge state={dataSource.status} />
          <Button
            aria-label={`Remove data source ${dataSource.name ?? dataSourceId}`}
            onClick={() => {
              setShowDelete(true)
            }}
            size="icon-sm"
            variant="destructive"
          >
            <Trash2 className="size-3.5" />
          </Button>
        </div>
      </div>

      <h4 className="mb-1 text-xs font-medium text-muted-foreground">
        Recent ingestion jobs
      </h4>
      {isLoading && (
        <p className="text-sm text-muted-foreground">Loading jobs…</p>
      )}
      {isError && (
        <p className="text-sm text-destructive">
          Failed to load ingestion jobs: {error?.message}
        </p>
      )}
      {!isLoading && !isError && jobs.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No ingestion jobs have run yet.
        </p>
      )}
      {jobs.length > 0 && (
        <div className="space-y-2">
          {jobs.map((job) => (
            <IngestionJobRow job={job} key={job.ingestionJobId} />
          ))}
        </div>
      )}

      <DeleteConfirmationDialog
        description={
          <>
            {`This removes "${dataSource.name ?? dataSourceId}" from the knowledge base. It cannot be undone.`}
            {deleteDataSource.error && (
              <span className="mt-2 block text-destructive">
                {deleteDataSource.error.message}
              </span>
            )}
          </>
        }
        isPending={deleteDataSource.isPending}
        onConfirm={() => {
          void handleDelete()
        }}
        onOpenChange={setShowDelete}
        open={showDelete}
        title="Remove Data Source"
      />
    </div>
  )
}
