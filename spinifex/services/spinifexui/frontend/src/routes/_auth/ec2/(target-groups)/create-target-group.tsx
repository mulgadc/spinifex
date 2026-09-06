import { createFileRoute } from "@tanstack/react-router"

import { ec2VpcsQueryOptions } from "@/queries/ec2"

import { CreateTargetGroupPage } from "./-components/create-target-group-page"

export const Route = createFileRoute(
  "/_auth/ec2/(target-groups)/create-target-group",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2VpcsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Create Target Group | EC2 | Mulga",
      },
    ],
  }),
  component: CreateTargetGroupPage,
})
