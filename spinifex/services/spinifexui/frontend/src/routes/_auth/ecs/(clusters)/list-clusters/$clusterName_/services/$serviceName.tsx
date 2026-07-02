import { createFileRoute } from "@tanstack/react-router"

import { ecsServicesQueryOptions, ecsTasksQueryOptions } from "@/queries/ecs"

import { ServiceDetailPage } from "../../../-components/service-detail-page"

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/list-clusters/$clusterName_/services/$serviceName",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(
        ecsServicesQueryOptions(params.clusterName),
      ),
      context.queryClient.ensureQueryData(
        ecsTasksQueryOptions(params.clusterName),
      ),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.serviceName} | ECS | Mulga` }],
  }),
  component: ServiceDetail,
})

function ServiceDetail() {
  const { clusterName, serviceName } = Route.useParams()
  return (
    <ServiceDetailPage clusterName={clusterName} serviceName={serviceName} />
  )
}
