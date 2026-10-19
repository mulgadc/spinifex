import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { knowledgeBasesQueryOptions } from "@/queries/bedrockAgent"

export function KnowledgeBasesPage() {
  const { data } = useSuspenseQuery(knowledgeBasesQueryOptions)

  const knowledgeBases = (data.knowledgeBaseSummaries ?? []).toSorted(
    byName((kb) => kb.name),
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

      <ResourceList
        empty="No knowledge bases found."
        getKey={(kb) => kb.knowledgeBaseId}
        items={knowledgeBases}
        renderItem={(kb, knowledgeBaseId) => (
          <ListCard
            badge={<StateBadge state={kb.status} />}
            params={{ knowledgeBaseId }}
            subtitle={knowledgeBaseId}
            title={kb.name ?? knowledgeBaseId}
            to="/bedrock/list-knowledge-bases/$knowledgeBaseId"
          />
        )}
      />
    </>
  )
}
