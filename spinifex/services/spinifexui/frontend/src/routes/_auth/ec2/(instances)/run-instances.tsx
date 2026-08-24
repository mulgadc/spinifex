import { createFileRoute } from "@tanstack/react-router"
import { z } from "zod"

import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2LaunchTemplatesQueryOptions,
  ec2PlacementGroupsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"

import { RunInstancesPage } from "./-components/run-instances-page"

/* oxlint-disable promise/prefer-await-to-then, unicorn/no-useless-undefined -- zod's .catch() supplies a schema fallback, not a promise handler */
const searchSchema = z.object({
  launchTemplateId: z.string().optional().catch(undefined),
  launchTemplateVersion: z.string().optional().catch(undefined),
})
/* oxlint-enable promise/prefer-await-to-then, unicorn/no-useless-undefined */

export const Route = createFileRoute("/_auth/ec2/(instances)/run-instances")({
  validateSearch: searchSchema,
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
      context.queryClient.ensureQueryData(ec2KeyPairsQueryOptions),
      context.queryClient.ensureQueryData(ec2InstanceTypesQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2PlacementGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2VpcsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
      context.queryClient.ensureQueryData(ec2LaunchTemplatesQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Run Instances | EC2 | Mulga",
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { launchTemplateId, launchTemplateVersion } = Route.useSearch()
  return (
    <RunInstancesPage
      launchTemplateId={launchTemplateId}
      launchTemplateVersion={launchTemplateVersion}
    />
  )
}
