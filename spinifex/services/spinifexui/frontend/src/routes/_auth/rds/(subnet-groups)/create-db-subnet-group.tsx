import { createFileRoute } from "@tanstack/react-router"

import { ec2SubnetsQueryOptions, ec2VpcsQueryOptions } from "@/queries/ec2"

import { CreateDBSubnetGroupPage } from "./-components/create-db-subnet-group-page"

export const Route = createFileRoute(
  "/_auth/rds/(subnet-groups)/create-db-subnet-group",
)({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2VpcsQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Create Subnet Group | RDS | Mulga",
      },
    ],
  }),
  component: CreateDBSubnetGroupPage,
})
