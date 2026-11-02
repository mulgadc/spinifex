import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  iamAttachedGroupPoliciesQueryOptions,
  iamGroupPoliciesQueryOptions,
  iamGroupQueryOptions,
  iamPoliciesQueryOptions,
  iamUsersQueryOptions,
} from "@/queries/iam"

import { GroupDetails } from "../-components/group-details"

export const Route = createFileRoute(
  "/_auth/iam/(groups)/list-groups/$groupName",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...iamGroupQueryOptions(params.groupName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...iamAttachedGroupPoliciesQueryOptions(params.groupName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...iamPoliciesQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...iamUsersQueryOptions,
        staleTime: "static",
      }),
      context.queryClient.query({
        ...iamGroupPoliciesQueryOptions(params.groupName),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.groupName} | IAM | Mulga` }],
  }),
  component: GroupDetail,
})

// The group's own page: the side panel's tabs, laid out for the full width.
function GroupDetail() {
  const { groupName } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(iamGroupQueryOptions(groupName))
  const group = data.Group

  if (!group) {
    return (
      <>
        <BackLink to="/iam/list-groups">Back to groups</BackLink>
        <p className="text-muted-foreground">Group not found.</p>
      </>
    )
  }

  return (
    <GroupDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/iam/list-groups" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      group={group}
      tab={tab}
    />
  )
}
