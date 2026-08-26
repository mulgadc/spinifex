import type {
  DataSourceSummary,
  IngestionJobSummary,
} from "@aws-sdk/client-bedrock-agent"
import { useQuery } from "@tanstack/react-query"

import { StateBadge } from "@/components/state-badge"
import { formatDateTime } from "@/lib/utils"
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
  const { data, isLoading, isError, error } = useQuery(
    ingestionJobsQueryOptions(knowledgeBaseId, dataSourceId),
  )

  const jobs = (data?.ingestionJobSummaries ?? []).slice(0, RECENT_JOBS_SHOWN)

  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="mb-2 flex items-center justify-between">
        <div>
          <h3 className="font-medium">{dataSource.name}</h3>
          <p className="font-mono text-xs text-muted-foreground">
            {dataSourceId}
          </p>
        </div>
        <StateBadge state={dataSource.status} />
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
    </div>
  )
}
