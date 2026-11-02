import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/s3/(buckets)/create-bucket")({
  beforeLoad: () => {
    throw redirect({ to: "/s3/ls", search: { create: true } })
  },
})
