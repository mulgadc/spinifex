import { createFileRoute } from "@tanstack/react-router"

import { ServiceMetricsPage } from "./-components/service-metrics-page"

export const Route = createFileRoute("/_auth/s3/service-metrics")({
  head: () => ({
    meta: [{ title: "Service Metrics | S3 | Mulga" }],
  }),
  component: ServiceMetricsPage,
})
