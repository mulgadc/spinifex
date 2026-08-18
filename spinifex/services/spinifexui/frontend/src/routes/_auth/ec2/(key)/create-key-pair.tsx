import { createFileRoute } from "@tanstack/react-router"

import { CreateKeyPairPage } from "./-components/create-key-pair-page"

export const Route = createFileRoute("/_auth/ec2/(key)/create-key-pair")({
  head: () => ({
    meta: [
      {
        title: "Create Key Pair | EC2 | Mulga",
      },
    ],
  }),
  component: CreateKeyPairPage,
})
