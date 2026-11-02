import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  iamAttachedRolePoliciesQueryOptions,
  iamInstanceProfilesForRoleQueryOptions,
  iamPoliciesQueryOptions,
  iamRolePoliciesQueryOptions,
  iamRoleQueryOptions,
} from "@/queries/iam"

import { RoleDetails } from "../-components/role-details"

export const Route = createFileRoute("/_auth/iam/(roles)/list-roles/$roleName")(
  {
    validateSearch: z.object({ tab: panelSearchFields.tab }),
    search: { middlewares: [stripSearchParams({ tab: "overview" })] },
    loader: async ({ context, params }) => {
      await Promise.all([
        context.queryClient.query({
          ...iamRoleQueryOptions(params.roleName),
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamAttachedRolePoliciesQueryOptions(params.roleName),
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamInstanceProfilesForRoleQueryOptions(params.roleName),
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamPoliciesQueryOptions,
          staleTime: "static",
        }),
        context.queryClient.query({
          ...iamRolePoliciesQueryOptions(params.roleName),
          staleTime: "static",
        }),
      ])
    },
    head: ({ params }) => ({
      meta: [{ title: `${params.roleName} | IAM | Mulga` }],
    }),
    component: RoleDetail,
  },
)

// The role's own page: the side panel's tabs, laid out for the full width.
function RoleDetail() {
  const { roleName } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(iamRoleQueryOptions(roleName))
  const role = data.Role

  if (!role) {
    return (
      <>
        <BackLink to="/iam/list-roles">Back to roles</BackLink>
        <p className="text-muted-foreground">Role not found.</p>
      </>
    )
  }

  return (
    <RoleDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/iam/list-roles" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      role={role}
      tab={tab}
    />
  )
}
