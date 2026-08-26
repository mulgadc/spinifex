import { useSuspenseQuery } from "@tanstack/react-query"

import { BackLink } from "@/components/back-link"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { formatDateTime } from "@/lib/utils"
import {
  dataSourcesQueryOptions,
  knowledgeBaseQueryOptions,
} from "@/queries/bedrockAgent"

import { DataSourceCard } from "./data-source-card"
import { RetrieveTester } from "./retrieve-tester"

interface KnowledgeBaseDetailPageProps {
  knowledgeBaseId: string
}

export function KnowledgeBaseDetailPage({
  knowledgeBaseId,
}: KnowledgeBaseDetailPageProps) {
  const { data: kbData } = useSuspenseQuery(
    knowledgeBaseQueryOptions(knowledgeBaseId),
  )
  const { data: dataSourcesData } = useSuspenseQuery(
    dataSourcesQueryOptions(knowledgeBaseId),
  )

  const { knowledgeBase } = kbData
  const dataSources = dataSourcesData.dataSourceSummaries ?? []

  if (!knowledgeBase) {
    return (
      <>
        <BackLink to="/bedrock/list-knowledge-bases">
          Back to knowledge bases
        </BackLink>
        <p className="text-muted-foreground">Knowledge base not found.</p>
      </>
    )
  }

  const embeddingModelArn =
    knowledgeBase.knowledgeBaseConfiguration?.vectorKnowledgeBaseConfiguration
      ?.embeddingModelArn

  return (
    <>
      <BackLink to="/bedrock/list-knowledge-bases">
        Back to knowledge bases
      </BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={<StateBadge state={knowledgeBase.status} />}
          subtitle="Knowledge Base Details"
          title={knowledgeBase.name ?? knowledgeBaseId}
        />

        <DetailCard>
          <DetailCard.Header>Knowledge Base Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow
              label="Knowledge Base ID"
              value={knowledgeBase.knowledgeBaseId}
            />
            <DetailRow label="ARN" value={knowledgeBase.knowledgeBaseArn} />
            <DetailRow
              label="Description"
              value={knowledgeBase.description ?? "-"}
            />
            <DetailRow label="Role ARN" value={knowledgeBase.roleArn} />
            <DetailRow label="Embedding Model" value={embeddingModelArn} />
            <DetailRow
              label="Created"
              value={formatDateTime(knowledgeBase.createdAt)}
            />
            <DetailRow
              label="Updated"
              value={formatDateTime(knowledgeBase.updatedAt)}
            />
            {knowledgeBase.failureReasons &&
              knowledgeBase.failureReasons.length > 0 && (
                <DetailRow
                  label="Failure Reasons"
                  value={knowledgeBase.failureReasons.join("; ")}
                />
              )}
          </DetailCard.Content>
        </DetailCard>

        <div>
          <h2 className="mb-3 font-semibold">Data Sources</h2>
          {dataSources.length > 0 ? (
            <div className="space-y-4">
              {dataSources.map((dataSource) => (
                <DataSourceCard
                  dataSource={dataSource}
                  key={dataSource.dataSourceId}
                  knowledgeBaseId={knowledgeBaseId}
                />
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground">
              No data sources configured for this knowledge base.
            </p>
          )}
        </div>

        <RetrieveTester knowledgeBaseId={knowledgeBaseId} />
      </div>
    </>
  )
}
