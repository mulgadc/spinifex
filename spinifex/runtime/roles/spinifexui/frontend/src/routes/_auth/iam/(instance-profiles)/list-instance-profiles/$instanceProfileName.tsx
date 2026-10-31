import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  iamInstanceProfileQueryOptions,
  iamRolesQueryOptions,
} from "@/queries/iam"

import { InstanceProfileDetails } from "../-components/instance-profile-details"

export const Route = createFileRoute(
  "/_auth/iam/(instance-profiles)/list-instance-profiles/$instanceProfileName",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...iamInstanceProfileQueryOptions(params.instanceProfileName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...iamRolesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.instanceProfileName} | IAM | Mulga` }],
  }),
  component: InstanceProfileDetail,
})

// The instance profile's own page: the side panel's tabs, laid out for the full width.
function InstanceProfileDetail() {
  const { instanceProfileName } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()

  const { data } = useSuspenseQuery(
    iamInstanceProfileQueryOptions(instanceProfileName),
  )

  const profile = data.InstanceProfile

  if (!profile) {
    return (
      <>
        <BackLink to="/iam/list-instance-profiles">
          Back to instance profiles
        </BackLink>
        <p className="text-muted-foreground">Instance profile not found.</p>
      </>
    )
  }

  return (
    <InstanceProfileDetails
      layout="page"
      onClose={() => {
        void navigate({ to: "/iam/list-instance-profiles" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      profile={profile}
      tab={tab}
    />
  )
}
