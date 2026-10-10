import { createFileRoute, Outlet } from "@tanstack/react-router"

export const Route = createFileRoute("/_auth/s3/ls/$bucket")({
  component: () => <Outlet />,
})
