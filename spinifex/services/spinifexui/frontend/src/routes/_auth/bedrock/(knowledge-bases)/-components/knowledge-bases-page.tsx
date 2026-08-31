import type { KnowledgeBaseSummary } from "@aws-sdk/client-bedrock-agent"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { knowledgeBasesQueryOptions } from "@/queries/bedrockAgent"

export function KnowledgeBasesPage() {
  const { data } = useSuspenseQuery(knowledgeBasesQueryOptions)

  const knowledgeBases = (data.knowledgeBaseSummaries ?? []).toSorted(
    (a, b) => {
      const nameA = a.name?.toLowerCase() ?? ""
      const nameB = b.name?.toLowerCase() ?? ""
      return nameA.localeCompare(nameB)
    },
  )

  return (
    <>
      <PageHeading
        actions={
          <Link
            className={buttonVariants({ size: "sm" })}
            to="/bedrock/list-knowledge-bases/create"
          >
            Create knowledge base
          </Link>
        }
        title="Knowledge Bases"
      />

      {knowledgeBases.length > 0 ? (
        <div className="space-y-4">
          {knowledgeBases.map((kb: KnowledgeBaseSummary) => {
            if (!kb.knowledgeBaseId) {
              return null
            }
            return (
              <ListCard
                badge={<StateBadge state={kb.status} />}
                key={kb.knowledgeBaseId}
                params={{ knowledgeBaseId: kb.knowledgeBaseId }}
                subtitle={kb.knowledgeBaseId}
                title={kb.name ?? kb.knowledgeBaseId}
                to="/bedrock/list-knowledge-bases/$knowledgeBaseId"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No knowledge bases found.</p>
      )}
    </>
  )
}
