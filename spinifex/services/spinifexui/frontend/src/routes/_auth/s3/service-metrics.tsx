import { createFileRoute } from "@tanstack/react-router"

import { RequireAdmin } from "@/components/require-admin"

import { ServiceMetricsPage } from "./-components/service-metrics-page"

export const Route = createFileRoute("/_auth/s3/service-metrics")({
  head: () => ({
    meta: [{ title: "Service Metrics | S3 | Mulga" }],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  return (
    <RequireAdmin>
      <ServiceMetricsPage />
    </RequireAdmin>
  )
}
