import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { guardrailsQueryOptions } from "@/queries/bedrock"

export function GuardrailsPage() {
  const { data } = useSuspenseQuery(guardrailsQueryOptions)

  const guardrails = (data.guardrails ?? []).toSorted(byName((g) => g.name))

  return (
    <>
      <PageHeading
        actions={
          <Link
            className={buttonVariants({ size: "sm" })}
            to="/bedrock/list-guardrails/create"
          >
            Create guardrail
          </Link>
        }
        title="Guardrails"
      />

      <ResourceList
        empty="No guardrails found."
        getKey={(guardrail) => guardrail.id}
        items={guardrails}
        renderItem={(guardrail, guardrailId) => (
          <ListCard
            badge={<StateBadge state={guardrail.status} />}
            params={{ guardrailId }}
            subtitle={guardrailId}
            title={guardrail.name ?? guardrailId}
            to="/bedrock/list-guardrails/$guardrailId"
          />
        )}
      />
    </>
  )
}
