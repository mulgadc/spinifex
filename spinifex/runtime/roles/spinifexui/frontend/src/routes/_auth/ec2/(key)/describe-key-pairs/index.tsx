import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { ec2KeyPairsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(key)/describe-key-pairs/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2KeyPairsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Key Pairs | EC2 | Mulga",
      },
    ],
  }),
  component: KeyPairs,
})

function KeyPairs() {
  const { data } = useSuspenseQuery(ec2KeyPairsQueryOptions)

  const keyPairs = (data.KeyPairs ?? []).toSorted(byName((k) => k.KeyName))

  return (
    <>
      <PageHeading
        actions={
          <div className="flex gap-2">
            <Link to="/ec2/import-key-pair">
              <Button variant="outline">Import Key Pair</Button>
            </Link>
            <Link to="/ec2/create-key-pair">
              <Button>Create Key Pair</Button>
            </Link>
          </div>
        }
        title="Key Pairs"
      />

      <ResourceList
        empty="No key pairs found."
        getKey={(keyPair) => keyPair.KeyPairId}
        items={keyPairs}
        renderItem={(keyPair, id) => (
          <ListCard
            params={{ id }}
            subtitle={id}
            title={keyPair.KeyName ?? ""}
            to="/ec2/describe-key-pairs/$id"
          />
        )}
      />
    </>
  )
}
