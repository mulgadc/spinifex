import { useQuery } from "@tanstack/react-query"
import { RefreshCw } from "lucide-react"

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
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
      <ul className="divide-y rounded-md border bg-card">
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
    <div className="flex flex-col gap-3">
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
          fallback={activationQuery.isPending ? "loading…" : "unknown"}
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
          <RefreshCw data-icon="inline-start" />
          {verifyMutation.isPending ? "Checking…" : "Check now"}
        </Button>
      </div>

      {activation && (
        <>
          <PanelSection title="Activation">
            <PanelFields>
              {showZoneId && (
                <PanelField copy={zoneId} label="Hosted zone ID" mono>
                  {zoneId}
                </PanelField>
              )}
              <PanelField label="Last check">
                {activation.checkedAt
                  ? formatDateTime(activation.checkedAt)
                  : "Not checked yet"}
              </PanelField>
              <PanelField label="Reason">{activation.reason}</PanelField>
              {activation.override && (
                <PanelField label="Operator override">
                  {activation.override}
                </PanelField>
              )}
            </PanelFields>
          </PanelSection>
          <PanelSection title="Expected name servers">
            <NameServerList nameServers={activation.nameServers} />
          </PanelSection>
          <PanelSection title="What the parent returned">
            <ParentAnswer
              delegatedTo={activation.delegatedTo}
              source={activation.delegationSource}
            />
          </PanelSection>
        </>
      )}
    </div>
  )
}
