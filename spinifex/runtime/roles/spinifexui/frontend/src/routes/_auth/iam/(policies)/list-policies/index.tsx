import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { iamPoliciesQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(policies)/list-policies/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamPoliciesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Policies | IAM | Mulga" }],
  }),
  component: Policies,
})

function Policies() {
  const { data } = useSuspenseQuery(iamPoliciesQueryOptions)

  const policies = (data.Policies ?? []).toSorted(byName((p) => p.PolicyName))

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

      <ResourceList
        empty="No IAM policies found."
        getKey={(policy) => policy.Arn}
        items={policies}
        renderItem={(policy, arn) => (
          <ListCard
            params={{ policyArn: encodeURIComponent(arn) }}
            subtitle={arn}
            title={policy.PolicyName ?? ""}
            to="/iam/list-policies/$policyArn"
          />
        )}
      />
    </>
  )
}
