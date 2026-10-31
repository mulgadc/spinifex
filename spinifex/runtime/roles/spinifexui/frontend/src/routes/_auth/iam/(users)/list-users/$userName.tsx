import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  iamAccessKeysQueryOptions,
  iamAttachedUserPoliciesQueryOptions,
  iamGroupsForUserQueryOptions,
  iamPoliciesQueryOptions,
  iamUserPoliciesQueryOptions,
  iamUserQueryOptions,
} from "@/queries/iam"

import { UserDetails } from "../-components/user-details"

export const Route = createFileRoute("/_auth/iam/(users)/list-users/$userName")(
  {
    validateSearch: z.object({ tab: panelSearchFields.tab }),
    search: { middlewares: [stripSearchParams({ tab: "overview" })] },
    loader: async ({ context, params }) => {
      await Promise.all([
        context.queryClient.query({
          ...iamUserQueryOptions(params.userName),
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamAccessKeysQueryOptions(params.userName),
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamAttachedUserPoliciesQueryOptions(params.userName),
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamPoliciesQueryOptions,
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamGroupsForUserQueryOptions(params.userName),
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamUserPoliciesQueryOptions(params.userName),
          staleTime: "static",
        }),
      ])
    },
    head: ({ params }) => ({
      meta: [{ title: `${params.userName} | IAM | Mulga` }],
    }),
    component: UserDetail,
  },
)

// The user's own page: the side panel's tabs, laid out for the full width.
function UserDetail() {
  const { userName } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(iamUserQueryOptions(userName))
  const user = data.User

  if (!user) {
    return (
      <>
        <BackLink to="/iam/list-users">Back to users</BackLink>
        <p className="text-muted-foreground">User not found.</p>
      </>
    )
  }

  return (
    <UserDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/iam/list-users" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
      user={user}
    />
  )
}
