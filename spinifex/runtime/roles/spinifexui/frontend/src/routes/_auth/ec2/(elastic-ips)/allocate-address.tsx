import { createFileRoute } from "@tanstack/react-router"

import { AllocateAddressPage } from "./-components/allocate-address-page"

export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/allocate-address",
)({
  head: () => ({
    meta: [
      {
        title: "Allocate Elastic IP | EC2 | Mulga",
      },
    ],
  }),
  component: AllocateAddress,
})

function AllocateAddress() {
  return <AllocateAddressPage />
}
