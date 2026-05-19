import type {
  TargetDescription,
  TargetHealthDescription,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { RegisterTargetsDialog } from "@/components/elbv2/register-targets-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"
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

const HEALTHY_STATES = new Set(["healthy"])
const UNHEALTHY_STATES = new Set(["unhealthy", "unavailable"])
const NEUTRAL_STATES = new Set(["initial", "draining"])

function healthChipClass(state: string | undefined): string {
  if (state && HEALTHY_STATES.has(state)) {
    return "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-100"
  }
  if (state && UNHEALTHY_STATES.has(state)) {
    return "bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-100"
  }
  if (state && NEUTRAL_STATES.has(state)) {
    return "bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-100"
  }
  return "bg-gray-100 text-gray-800 dark:bg-gray-800 dark:text-gray-100"
}

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
        <Button onClick={() => setRegisterOpen(true)} size="sm">
          Register targets
        </Button>
      </div>

      {descriptions.length === 0 ? (
        <p className="text-muted-foreground">No targets registered.</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Target</th>
                <th className="px-4 py-2 font-medium">Port</th>
                <th className="px-4 py-2 font-medium">AZ</th>
                <th className="px-4 py-2 font-medium">Health</th>
                <th className="px-4 py-2 font-medium">Reason</th>
                <th className="px-4 py-2 font-medium">Description</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {descriptions.map((desc) => {
                const target: TargetDescription = desc.Target ?? { Id: "" }
                const state = desc.TargetHealth?.State
                return (
                  <tr
                    className="border-b last:border-0"
                    key={targetKey(target)}
                  >
                    <td className="px-4 py-2 font-mono text-xs">
                      {target.Id ?? ""}
                    </td>
                    <td className="px-4 py-2">{target.Port ?? ""}</td>
                    <td className="px-4 py-2 text-xs">
                      {target.AvailabilityZone ?? ""}
                    </td>
                    <td className="px-4 py-2">
                      <span
                        className={cn(
                          "rounded-full px-2 py-0.5 text-xs",
                          healthChipClass(state),
                        )}
                      >
                        {state ?? "unknown"}
                      </span>
                    </td>
                    <td className="px-4 py-2 text-xs">
                      {desc.TargetHealth?.Reason ?? ""}
                    </td>
                    <td className="px-4 py-2 text-xs">
                      {desc.TargetHealth?.Description ?? ""}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <Button
                        aria-label={`Deregister ${target.Id}`}
                        onClick={() => setDeregisterTarget(target)}
                        size="sm"
                        variant="ghost"
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <RegisterTargetsDialog
        defaultPort={defaultPort}
        instances={vpcInstances}
        isPending={registerMutation.isPending}
        onConfirm={handleRegister}
        onOpenChange={setRegisterOpen}
        open={registerOpen}
      />

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
        onOpenChange={(open) => !open && setDeregisterTarget(undefined)}
        open={deregisterTarget !== undefined}
        title="Deregister target"
      />
    </div>
  )
}
