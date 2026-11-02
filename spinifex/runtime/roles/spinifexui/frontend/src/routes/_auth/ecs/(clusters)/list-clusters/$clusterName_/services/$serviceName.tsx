import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ecsServicesQueryOptions, ecsTasksQueryOptions } from "@/queries/ecs"

import { ServiceDetails } from "../../../-components/service-details"

export const Route = createFileRoute(
  "/_auth/ecs/(clusters)/list-clusters/$clusterName_/services/$serviceName",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ecsServicesQueryOptions(params.clusterName),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ecsTasksQueryOptions(params.clusterName),
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [{ title: `${params.serviceName} | ECS | Mulga` }],
  }),
  component: ServiceDetail,
})

// The service's own page, inside its cluster.
function ServiceDetail() {
  const { clusterName, serviceName } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()

  const { data: services } = useSuspenseQuery(
    ecsServicesQueryOptions(clusterName),
  )

  const { data: tasks } = useSuspenseQuery(ecsTasksQueryOptions(clusterName))
  const service = services.find((s) => s.serviceName === serviceName)

  if (!service) {
    return (
      <>
        <BackLink params={{ clusterName }} to="/ecs/list-clusters/$clusterName">
          {clusterName}
        </BackLink>
        <p className="text-muted-foreground">Service not found.</p>
      </>
    )
  }

  return (
    <ServiceDetails
      clusterName={clusterName}
      layout="page"
      onClose={() => {
        void navigate({
          to: "/ecs/list-clusters/$clusterName",
          params: { clusterName },
        })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      service={service}
      tab={tab}
      tasks={tasks}
    />
  )
}
