import { createFileRoute } from "@tanstack/react-router"

import { CreateInternetGatewayPage } from "./-components/create-internet-gateway-page"

export const Route = createFileRoute(
  "/_auth/ec2/(internet-gateways)/create-internet-gateway",
)({
  head: () => ({
    meta: [
      {
        title: "Create Internet Gateway | EC2 | Mulga",
      },
    ],
  }),
  component: RouteComponent,
})

function RouteComponent() {
  return <CreateInternetGatewayPage />
}
