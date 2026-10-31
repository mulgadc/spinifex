import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { safeDecodeURIComponent } from "@/lib/utils"
import {
  iamPolicyQueryOptions,
  iamPolicyVersionQueryOptions,
} from "@/queries/iam"

import { PolicyDetails } from "../-components/policy-details"

export const Route = createFileRoute(
  "/_auth/iam/(policies)/list-policies/$policyArn",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    const policyArn = safeDecodeURIComponent(params.policyArn)

    const policyData = await context.queryClient.query({
      ...iamPolicyQueryOptions(policyArn),
      staleTime: "static",
    })

    const versionId = policyData.Policy?.DefaultVersionId

    if (versionId) {
      await context.queryClient.query({
        ...iamPolicyVersionQueryOptions(policyArn, versionId),
        staleTime: "static",
      })
    }

    return policyData
  },
  head: ({ loaderData }) => ({
    meta: [
      {
        title: `${loaderData?.Policy?.PolicyName ?? "Policy"} | IAM | Mulga`,
      },
    ],
  }),
  component: PolicyDetail,
})

// The policy's own page: the side panel's tabs, laid out for the full width.
function PolicyDetail() {
  const { policyArn: encodedArn } = Route.useParams()
  const policyArn = safeDecodeURIComponent(encodedArn)
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(iamPolicyQueryOptions(policyArn))
  const policy = data.Policy

  if (!policy) {
    return (
      <>
        <BackLink to="/iam/list-policies">Back to policies</BackLink>
        <p className="text-muted-foreground">Policy not found.</p>
      </>
    )
  }

  return (
    <PolicyDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/iam/list-policies" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      policy={policy}
      tab={tab}
    />
  )
}
