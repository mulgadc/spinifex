import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2LaunchTemplatesQueryOptions,
  ec2LaunchTemplateVersionsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"

import { LaunchTemplateDetails } from "../-components/launch-template-details"

export const Route = createFileRoute(
  "/_auth/ec2/(launch-templates)/describe-launch-templates/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all(
      [
        ec2LaunchTemplatesQueryOptions,
        ec2LaunchTemplateVersionsQueryOptions(params.id),
        ec2ImagesQueryOptions,
        ec2InstanceTypesQueryOptions,
        ec2KeyPairsQueryOptions,
        ec2SubnetsQueryOptions,
        ec2SecurityGroupsQueryOptions,
      ].map(
        async (options) =>
          await context.queryClient.query({ ...options, staleTime: "static" }),
      ),
    )
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Launch Template | Mulga`,
      },
    ],
  }),
  component: LaunchTemplateDetail,
})

// The template's own page: the side panel's tabs, laid out for the full width.
function LaunchTemplateDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
  const { data } = useSuspenseQuery(ec2LaunchTemplatesQueryOptions)
  const lt = data.LaunchTemplates?.find((t) => t.LaunchTemplateId === id)

  if (!lt?.LaunchTemplateId) {
    return (
      <>
        <BackLink to="/ec2/describe-launch-templates">
          Back to launch templates
        </BackLink>
        <p className="text-muted-foreground">Launch template not found.</p>
      </>
    )
  }

  return (
    <LaunchTemplateDetails
      launchTemplate={lt}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-launch-templates" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
