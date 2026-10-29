import { createFileRoute } from "@tanstack/react-router"

import { CreateVpcPage } from "./-components/create-vpc-page"

export const Route = createFileRoute("/_auth/ec2/(vpc)/create-vpc")({
  head: () => ({
    meta: [
      {
        title: "Create VPC | EC2 | Mulga",
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  return <CreateVpcPage />
}
