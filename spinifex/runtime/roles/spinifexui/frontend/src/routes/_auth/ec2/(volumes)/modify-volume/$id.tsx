import { createFileRoute, redirect } from "@tanstack/react-router"

export const Route = createFileRoute("/_auth/ec2/(volumes)/modify-volume/$id")({
  beforeLoad: ({ params }) => {
    throw redirect({
      to: "/ec2/describe-volumes",
      search: { selected: params.id, tab: "settings" },
    })
  },
})
