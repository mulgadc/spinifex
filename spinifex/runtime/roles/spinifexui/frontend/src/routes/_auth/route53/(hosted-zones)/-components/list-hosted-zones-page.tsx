import { useQueries, useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"

import { PageHeading } from "@/components/page-heading"
import { ActivationBadge } from "@/components/route53/activation-badge"
import { Button } from "@/components/ui/button"
import { bareZoneId } from "@/lib/route53-activation"
import {
  route53ActivationQueryOptions,
  route53HostedZonesQueryOptions,
} from "@/queries/route53"

export function ListHostedZonesPage() {
  const navigate = useNavigate()
  const { data: zones } = useSuspenseQuery(route53HostedZonesQueryOptions)

  const activations = useQueries({
    queries: zones.map((zone) =>
      route53ActivationQueryOptions(bareZoneId(zone.Id ?? "")),
    ),
  })

  return (
    <>
      <PageHeading
        actions={
          <Button
            onClick={async () => {
              await navigate({ to: "/route53/create-hosted-zone" })
            }}
          >
            Add domain
          </Button>
        }
        subtitle="Domains whose DNS this cluster serves"
        title="Hosted zones"
      />

      {zones.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Domain name</th>
                <th className="px-4 py-2 font-medium">Status</th>
                <th className="px-4 py-2 font-medium">Records</th>
                <th className="px-4 py-2 font-medium">Comment</th>
                <th className="px-4 py-2 font-medium">Hosted zone ID</th>
              </tr>
            </thead>
            <tbody>
              {zones.map((zone, index) => {
                const id = bareZoneId(zone.Id ?? "")
                const activation = activations[index]

                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={id}
                    onClick={async () => {
                      await navigate({
                        to: "/route53/list-hosted-zones/$id",
                        params: { id },
                      })
                    }}
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => {
                          e.stopPropagation()
                        }}
                        params={{ id }}
                        to="/route53/list-hosted-zones/$id"
                      >
                        {zone.Name}
                      </Link>
                    </td>
                    <td className="px-4 py-2">
                      <ActivationBadge
                        fallback={
                          activation?.isPending ? "Loading…" : "Unknown"
                        }
                        state={activation?.data?.state}
                      />
                    </td>
                    <td className="px-4 py-2">
                      {zone.ResourceRecordSetCount ?? "—"}
                    </td>
                    <td className="px-4 py-2 text-muted-foreground">
                      {zone.Config?.Comment ?? ""}
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">{id}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="space-y-2">
          <p className="text-muted-foreground">No hosted zones yet.</p>
          <p className="text-sm text-muted-foreground">
            Add a domain to serve its DNS from this cluster. You will get the
            name servers to set at your registrar.
          </p>
        </div>
      )}
    </>
  )
}
