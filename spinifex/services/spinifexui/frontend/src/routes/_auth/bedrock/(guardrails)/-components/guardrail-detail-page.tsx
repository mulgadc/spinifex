import type { GuardrailSummary, GuardrailTopic } from "@aws-sdk/client-bedrock"
import { useSuspenseQuery } from "@tanstack/react-query"

import { BackLink } from "@/components/back-link"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { formatDateTime } from "@/lib/utils"
import {
  guardrailQueryOptions,
  guardrailVersionsQueryOptions,
} from "@/queries/bedrock"

import { GuardrailTester } from "./guardrail-tester"

interface GuardrailDetailPageProps {
  guardrailId: string
}

function DeniedTopicRow({ topic }: { topic: GuardrailTopic }) {
  return (
    <div className="rounded-md border p-3 text-sm">
      <div className="mb-1 flex items-center justify-between gap-2">
        <span className="font-semibold">{topic.name}</span>
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs">
          {topic.type ?? "DENY"}
        </span>
      </div>
      <p className="text-muted-foreground">{topic.definition}</p>
      {topic.examples && topic.examples.length > 0 && (
        <ul className="mt-2 list-inside list-disc text-xs text-muted-foreground">
          {topic.examples.map((example) => (
            <li key={example}>{example}</li>
          ))}
        </ul>
      )}
    </div>
  )
}

function VersionRow({ version }: { version: GuardrailSummary }) {
  return (
    <div className="flex items-center justify-between rounded-md border p-2 text-sm">
      <span className="font-mono">{version.version}</span>
      <StateBadge state={version.status} />
    </div>
  )
}

export function GuardrailDetailPage({ guardrailId }: GuardrailDetailPageProps) {
  const { data: guardrail } = useSuspenseQuery(
    guardrailQueryOptions(guardrailId),
  )
  const { data: versionsData } = useSuspenseQuery(
    guardrailVersionsQueryOptions(guardrailId),
  )

  const versions = versionsData.guardrails ?? []
  const deniedTopics = guardrail.topicPolicy?.topics ?? []

  return (
    <>
      <BackLink to="/bedrock/list-guardrails">Back to guardrails</BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={<StateBadge state={guardrail.status} />}
          subtitle="Guardrail Details"
          title={guardrail.name ?? guardrailId}
        />

        <DetailCard>
          <DetailCard.Header>Guardrail Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Guardrail ID" value={guardrail.guardrailId} />
            <DetailRow label="ARN" value={guardrail.guardrailArn} />
            <DetailRow label="Version" value={guardrail.version} />
            <DetailRow
              label="Description"
              value={guardrail.description ?? "-"}
            />
            <DetailRow
              label="Created"
              value={formatDateTime(guardrail.createdAt)}
            />
            <DetailRow
              label="Updated"
              value={formatDateTime(guardrail.updatedAt)}
            />
            {guardrail.statusReasons && guardrail.statusReasons.length > 0 && (
              <DetailRow
                label="Status Reasons"
                value={guardrail.statusReasons.join("; ")}
              />
            )}
          </DetailCard.Content>
        </DetailCard>

        <div>
          <h2 className="mb-3 font-semibold">Denied topics</h2>
          {deniedTopics.length > 0 ? (
            <div className="space-y-2">
              {deniedTopics.map((topic) => (
                <DeniedTopicRow key={topic.name} topic={topic} />
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground">
              No topic policy configured for this guardrail.
            </p>
          )}
        </div>

        <div>
          <h2 className="mb-3 font-semibold">Other policies</h2>
          <div className="flex flex-wrap gap-2">
            <span className="rounded-full bg-muted px-2 py-1 text-xs">
              Word policy:{" "}
              {(guardrail.wordPolicy?.words?.length ?? 0) > 0 ||
              (guardrail.wordPolicy?.managedWordLists?.length ?? 0) > 0
                ? "configured"
                : "not configured"}
            </span>
            <span className="rounded-full bg-muted px-2 py-1 text-xs">
              Sensitive information policy:{" "}
              {(guardrail.sensitiveInformationPolicy?.piiEntities?.length ??
                0) > 0 ||
              (guardrail.sensitiveInformationPolicy?.regexes?.length ?? 0) > 0
                ? "configured"
                : "not configured"}
            </span>
          </div>
        </div>

        <div>
          <h2 className="mb-3 font-semibold">Versions</h2>
          {versions.length > 0 ? (
            <div className="space-y-2">
              {versions.map((version) => (
                <VersionRow key={version.version} version={version} />
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground">
              No versions found for this guardrail.
            </p>
          )}
        </div>

        <GuardrailTester
          guardrailId={guardrailId}
          guardrailVersion={guardrail.version ?? "DRAFT"}
        />
      </div>
    </>
  )
}
