import { createFileRoute } from "@tanstack/react-router"

import { pageTitle } from "@/lib/branding"
import { requireAdmin } from "@/lib/require-admin"

import { ServiceMetricsPage } from "./-components/service-metrics-page"

export const Route = createFileRoute("/_auth/s3/service-metrics")({
  beforeLoad: requireAdmin,
  head: () => ({
    meta: [{ title: pageTitle("Service Metrics | S3") }],
  }),
  component: ServiceMetricsPage,
})
