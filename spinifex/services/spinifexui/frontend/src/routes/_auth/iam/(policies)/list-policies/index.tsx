import type { Policy } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { iamPoliciesQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(policies)/list-policies/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(iamPoliciesQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Policies | IAM | Mulga" }],
  }),
  component: Policies,
})

function Policies() {
  const { data } = useSuspenseQuery(iamPoliciesQueryOptions)

  const policies = (data.Policies ?? []).toSorted((a, b) => {
    const nameA = a.PolicyName?.toLowerCase() ?? ""
    const nameB = b.PolicyName?.toLowerCase() ?? ""
    return nameA.localeCompare(nameB)
  })

  return (
    <>
      <PageHeading
        actions={
          <Link to="/iam/create-policy">
            <Button>Create Policy</Button>
          </Link>
        }
        title="Policies"
      />

      {policies.length > 0 ? (
        <div className="space-y-4">
          {policies.map((policy: Policy) => {
            if (!policy.Arn) {
              return null
            }
            return (
              <ListCard
                key={policy.Arn}
                params={{ policyArn: encodeURIComponent(policy.Arn) }}
                subtitle={policy.Arn ?? ""}
                title={policy.PolicyName ?? ""}
                to="/iam/list-policies/$policyArn"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No IAM policies found.</p>
      )}
    </>
  )
}
