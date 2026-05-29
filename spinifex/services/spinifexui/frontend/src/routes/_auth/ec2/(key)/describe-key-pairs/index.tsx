import type { KeyPairInfo } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { ec2KeyPairsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(key)/describe-key-pairs/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2KeyPairsQueryOptions)
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

  const keyPairs = (data.KeyPairs ?? []).toSorted((a, b) => {
    const nameA = a.KeyName?.toLowerCase() ?? ""
    const nameB = b.KeyName?.toLowerCase() ?? ""
    return nameA.localeCompare(nameB)
  })

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

      {keyPairs.length > 0 ? (
        <div className="space-y-4">
          {keyPairs.map((keyPair: KeyPairInfo) => {
            if (!keyPair.KeyPairId) {
              return null
            }
            return (
              <ListCard
                key={keyPair.KeyPairId}
                params={{ id: keyPair.KeyPairId }}
                subtitle={keyPair.KeyPairId ?? ""}
                title={keyPair.KeyName ?? ""}
                to="/ec2/describe-key-pairs/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No key pairs found.</p>
      )}
    </>
  )
}
