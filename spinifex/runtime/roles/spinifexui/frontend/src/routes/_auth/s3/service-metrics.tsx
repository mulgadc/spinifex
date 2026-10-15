import { createFileRoute } from "@tanstack/react-router"

import { requireAdmin } from "@/lib/require-admin"

import { ServiceMetricsPage } from "./-components/service-metrics-page"

export const Route = createFileRoute("/_auth/s3/service-metrics")({
  beforeLoad: requireAdmin,
  head: () => ({
    meta: [{ title: "Service Metrics | S3 | Mulga" }],
  }),
  component: ServiceMetricsPage,
})
