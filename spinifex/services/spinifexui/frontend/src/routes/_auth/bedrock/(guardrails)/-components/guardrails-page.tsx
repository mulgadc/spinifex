import type { GuardrailSummary } from "@aws-sdk/client-bedrock"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { buttonVariants } from "@/components/ui/button"
import { guardrailsQueryOptions } from "@/queries/bedrock"

export function GuardrailsPage() {
  const { data } = useSuspenseQuery(guardrailsQueryOptions)

  const guardrails = (data.guardrails ?? []).toSorted((a, b) => {
    const nameA = a.name?.toLowerCase() ?? ""
    const nameB = b.name?.toLowerCase() ?? ""
    return nameA.localeCompare(nameB)
  })

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

      {guardrails.length > 0 ? (
        <div className="space-y-4">
          {guardrails.map((guardrail: GuardrailSummary) => {
            if (!guardrail.id) {
              return null
            }
            return (
              <ListCard
                badge={<StateBadge state={guardrail.status} />}
                key={guardrail.id}
                params={{ guardrailId: guardrail.id }}
                subtitle={guardrail.id}
                title={guardrail.name ?? guardrail.id}
                to="/bedrock/list-guardrails/$guardrailId"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No guardrails found.</p>
      )}
    </>
  )
}
