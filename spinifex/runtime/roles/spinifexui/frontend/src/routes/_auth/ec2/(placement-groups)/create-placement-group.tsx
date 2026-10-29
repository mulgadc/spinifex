import { createFileRoute } from "@tanstack/react-router"

import { CreatePlacementGroupPage } from "./-components/create-placement-group-page"

export const Route = createFileRoute(
  "/_auth/ec2/(placement-groups)/create-placement-group",
)({
  head: () => ({
    meta: [
      {
        title: "Create Placement Group | EC2 | Mulga",
      },
    ],
  }),
  component: CreatePlacementGroup,
})

function CreatePlacementGroup() {
  return <CreatePlacementGroupPage />
}
