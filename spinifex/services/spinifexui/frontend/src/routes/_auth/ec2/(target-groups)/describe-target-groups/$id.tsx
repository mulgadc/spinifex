import { createFileRoute } from "@tanstack/react-router"

import { ec2InstancesQueryOptions } from "@/queries/ec2"
import {
  elbv2TagsQueryOptions,
  elbv2TargetGroupAttributesQueryOptions,
  elbv2TargetGroupQueryOptions,
} from "@/queries/elbv2"

import { TargetGroupDetailPage } from "../-components/target-group-detail-page"

export const Route = createFileRoute(
  "/_auth/ec2/(target-groups)/describe-target-groups/$id",
)({
  loader: async ({ context, params }) => {
    const arn = decodeURIComponent(params.id)
    await Promise.all([
      context.queryClient.query({
        ...elbv2TargetGroupQueryOptions(arn),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2TargetGroupAttributesQueryOptions(arn),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...elbv2TagsQueryOptions([arn]),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2InstancesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${decodeURIComponent(params.id)} | Target Group | Mulga`,
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  const { id } = Route.useParams()
  return <TargetGroupDetailPage arn={decodeURIComponent(id)} />
}
