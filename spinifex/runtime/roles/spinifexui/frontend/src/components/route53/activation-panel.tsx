import { useQuery } from "@tanstack/react-query"
import { RefreshCw } from "lucide-react"

import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { ActivationBadge } from "@/components/route53/activation-badge"
import { NameServerList } from "@/components/route53/name-server-list"
import { Button } from "@/components/ui/button"
import { formatDateTime } from "@/lib/utils"
import { useVerifyHostedZoneDelegation } from "@/mutations/route53"
import { route53ActivationPollingQueryOptions } from "@/queries/route53"

interface ActivationPanelProps {
  zoneId: string
  // The zone ID is what Terraform and the CLI take, so the zone page shows it.
  showZoneId?: boolean
}

function ParentAnswer({
  delegatedTo,
  source,
}: {
  delegatedTo: readonly string[]
  source: string | undefined
}) {
  if (delegatedTo.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        The parent zone does not delegate this domain to any name servers yet.
      </p>
    )
  }

  return (
    <div className="space-y-2">
      <ul className="max-w-md divide-y rounded-md border bg-card">
        {delegatedTo.map((ns) => (
          <li className="px-3 py-2 font-mono text-sm" key={ns}>
            {ns}
          </li>
        ))}
      </ul>
      {source && (
        <p className="text-xs text-muted-foreground">Answered by {source}</p>
      )}
    </div>
  )
}

export function ActivationPanel({
  zoneId,
  showZoneId = false,
}: ActivationPanelProps) {
  const activationQuery = useQuery(route53ActivationPollingQueryOptions(zoneId))
  const verifyMutation = useVerifyHostedZoneDelegation()
  const activation = activationQuery.data

  return (
    <div className="space-y-4">
      {activationQuery.error && (
        <ErrorBanner
          error={activationQuery.error}
          msg="Failed to load the zone's activation"
        />
      )}
      {verifyMutation.error && (
        <ErrorBanner
          error={verifyMutation.error}
          msg="Failed to check the delegation"
        />
      )}

      <div className="flex items-center gap-3">
        <ActivationBadge
          fallback={activationQuery.isPending ? "Loading…" : "Unknown"}
          state={activation?.state}
        />
        <Button
          disabled={verifyMutation.isPending}
          onClick={() => {
            verifyMutation.mutate(zoneId)
          }}
          size="sm"
          type="button"
          variant="outline"
        >
          <RefreshCw className="size-4" />
          {verifyMutation.isPending ? "Checking…" : "Check now"}
        </Button>
      </div>

      {activation && (
        <>
          <DetailCard>
            <DetailCard.Header>Activation</DetailCard.Header>
            <DetailCard.Content>
              {showZoneId && (
                <DetailRow label="Hosted zone ID" value={zoneId} />
              )}
              <DetailRow
                label="Last check"
                value={
                  activation.checkedAt
                    ? formatDateTime(activation.checkedAt)
                    : "Not checked yet"
                }
              />
              <DetailRow label="Reason" value={activation.reason} />
              {activation.override && (
                <DetailRow
                  label="Operator override"
                  value={activation.override}
                />
              )}
            </DetailCard.Content>
          </DetailCard>

          <div className="grid gap-6 md:grid-cols-2">
            <section className="space-y-2">
              <h3 className="text-sm font-medium">Expected name servers</h3>
              <NameServerList nameServers={activation.nameServers} />
            </section>
            <section className="space-y-2">
              <h3 className="text-sm font-medium">What the parent returned</h3>
              <ParentAnswer
                delegatedTo={activation.delegatedTo}
                source={activation.delegationSource}
              />
            </section>
          </div>
        </>
      )}
    </div>
  )
}
