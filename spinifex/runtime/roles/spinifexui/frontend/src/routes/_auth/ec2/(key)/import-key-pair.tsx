import { createFileRoute, redirect } from "@tanstack/react-router"

// The form opens in the list's side panel; this address is kept for links.
export const Route = createFileRoute("/_auth/ec2/(key)/import-key-pair")({
  beforeLoad: () => {
    throw redirect({ to: "/ec2/describe-key-pairs", search: { import: true } })
  },
})
