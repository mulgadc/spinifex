import type {
  TargetDescription,
  TargetHealthDescription,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelField, PanelFields } from "@/components/detail-panel"
import { RegisterTargetsDialog } from "@/components/elbv2/register-targets-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import type { TargetInput } from "@/mutations/elbv2"
import { useDeregisterTargets, useRegisterTargets } from "@/mutations/elbv2"
import { ec2InstancesQueryOptions } from "@/queries/ec2"
import { elbv2TargetHealthQueryOptions } from "@/queries/elbv2"

interface TargetsTabProps {
  targetGroupArn: string
  vpcId: string | undefined
  defaultPort: number
  isActive: boolean
}

const HEALTH_POLL_MS = 5000

function targetKey(t: TargetDescription): string {
  return `${t.Id ?? ""}:${t.Port ?? ""}`
}

export function TargetsTab({
  targetGroupArn,
  vpcId,
  defaultPort,
  isActive,
}: TargetsTabProps) {
  const { data: instancesData } = useSuspenseQuery(ec2InstancesQueryOptions)

  const {
    data: healthData,
    isLoading,
    error: healthError,
  } = useQuery({
    ...elbv2TargetHealthQueryOptions(targetGroupArn),
    refetchInterval: isActive ? HEALTH_POLL_MS : false,
    refetchIntervalInBackground: false,
  })

  const registerMutation = useRegisterTargets()
  const deregisterMutation = useDeregisterTargets()

  const [registerOpen, setRegisterOpen] = useState(false)

  const [deregisterTarget, setDeregisterTarget] = useState<
    TargetDescription | undefined
  >()

  const vpcInstances = (
    instancesData.Reservations?.flatMap((r) => r.Instances ?? []) ?? []
  ).filter((i) => i.VpcId === vpcId && i.State?.Name !== "terminated")

  const descriptions: TargetHealthDescription[] =
    healthData?.TargetHealthDescriptions ?? []

  const handleRegister = async (targets: TargetInput[]) => {
    try {
      await registerMutation.mutateAsync({ targetGroupArn, targets })
      setRegisterOpen(false)
    } catch {
      // error surfaced below via mutation state
    }
  }

  const handleDeregister = async () => {
    if (!deregisterTarget?.Id) {
      return
    }

    try {
      await deregisterMutation.mutateAsync({
        targetGroupArn,
        targets: [
          {
            id: deregisterTarget.Id,
            port: deregisterTarget.Port,
          },
        ],
      })
    } finally {
      setDeregisterTarget(undefined)
    }
  }

  return (
    <div className="space-y-3">
      {registerMutation.error && (
        <ErrorBanner
          error={registerMutation.error}
          msg="Failed to register targets"
        />
      )}
      {deregisterMutation.error && (
        <ErrorBanner
          error={deregisterMutation.error}
          msg="Failed to deregister target"
        />
      )}
      {healthError && (
        <ErrorBanner error={healthError} msg="Failed to load target health" />
      )}

      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          {isLoading
            ? "Loading target health\u2026"
            : `${descriptions.length} target${
                descriptions.length === 1 ? "" : "s"
              }`}
        </p>
        <Button
          onClick={() => {
            setRegisterOpen(true)
          }}
          size="sm"
        >
          Register targets
        </Button>
      </div>

      {descriptions.length === 0 ? (
        <p className="text-sm text-muted-foreground">No targets registered.</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {descriptions.map((desc) => {
            const target: TargetDescription = desc.Target ?? { Id: "" }
            const state = desc.TargetHealth?.State

            return (
              <li className="rounded-md border bg-card" key={targetKey(target)}>
                <div className="flex items-center justify-between gap-2 py-1 pr-1 pl-3">
                  <span className="truncate font-mono text-xs">
                    {target.Id ?? ""}
                  </span>
                  <div className="flex items-center gap-2">
                    <StateBadge state={state ?? "unknown"} />
                    <Button
                      aria-label={`Deregister ${target.Id}`}
                      onClick={() => {
                        setDeregisterTarget(target)
                      }}
                      size="icon-sm"
                      variant="ghost"
                    >
                      <Trash2 />
                    </Button>
                  </div>
                </div>
                <div className="border-t px-3">
                  <PanelFields>
                    <PanelField label="Port">{target.Port}</PanelField>
                    <PanelField label="Availability zone">
                      {target.AvailabilityZone}
                    </PanelField>
                    {desc.TargetHealth?.Reason && (
                      <PanelField label="Reason">
                        {desc.TargetHealth.Reason}
                      </PanelField>
                    )}
                  </PanelFields>
                  {desc.TargetHealth?.Description && (
                    <p className="border-t py-1.5 text-xs break-words text-muted-foreground">
                      {desc.TargetHealth.Description}
                    </p>
                  )}
                </div>
              </li>
            )
          })}
        </ul>
      )}

      {registerOpen && (
        <RegisterTargetsDialog
          defaultPort={defaultPort}
          instances={vpcInstances}
          isPending={registerMutation.isPending}
          onConfirm={handleRegister}
          onOpenChange={setRegisterOpen}
        />
      )}

      <DeleteConfirmationDialog
        description={
          deregisterTarget
            ? `Deregister ${deregisterTarget.Id}${
                deregisterTarget.Port ? `:${deregisterTarget.Port}` : ""
              } from this target group?`
            : ""
        }
        isPending={deregisterMutation.isPending}
        onConfirm={handleDeregister}
        onOpenChange={(open) => {
          if (!open) {
            setDeregisterTarget(undefined)
          }
        }}
        open={deregisterTarget !== undefined}
        title="Deregister target"
      />
    </div>
  )
}
