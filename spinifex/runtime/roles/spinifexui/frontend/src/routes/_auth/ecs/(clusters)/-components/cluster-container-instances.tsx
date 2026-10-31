import type { ContainerInstance } from "@aws-sdk/client-ecs"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Server } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import {
  useDeregisterContainerInstance,
  useUpdateContainerInstanceState,
} from "@/mutations/ecs"
import { ecsContainerInstancesQueryOptions } from "@/queries/ecs"

import { PanelList, PanelListItem } from "./panel-list"
import { ProvisionCapacityDialog } from "./provision-capacity-dialog"

type Action = "drain" | "activate" | "deregister"

interface PendingAction {
  arn: string
  action: Action
}

const ACTION_LABELS = {
  drain: "Drain",
  activate: "Activate",
  deregister: "Deregister",
} satisfies Record<Action, string>

const ACTION_DESCRIPTIONS = {
  drain:
    "Draining force-stops the service-owned tasks on this instance so it can be retired.",
  activate:
    "This returns the instance to ACTIVE so the scheduler can place tasks on it again.",
  deregister:
    "This force-removes the container instance record. Any tasks it still holds are stopped.",
} satisfies Record<Action, string>

export function ClusterContainerInstances({
  clusterName,
}: {
  clusterName: string
}) {
  const { data: instances } = useSuspenseQuery(
    ecsContainerInstancesQueryOptions(clusterName),
  )

  const updateState = useUpdateContainerInstanceState()
  const deregister = useDeregisterContainerInstance()
  const [pending, setPending] = useState<PendingAction | null>(null)
  const [showProvision, setShowProvision] = useState(false)

  const onSuccess = () => {
    setPending(null)
  }

  function handleConfirm() {
    if (!pending) {
      return
    }

    if (pending.action === "deregister") {
      deregister.mutate(
        { cluster: clusterName, containerInstance: pending.arn },
        { onSuccess },
      )

      return
    }

    updateState.mutate(
      {
        cluster: clusterName,
        containerInstance: pending.arn,
        status: pending.action === "drain" ? "DRAINING" : "ACTIVE",
      },
      { onSuccess },
    )
  }

  const isError = updateState.isError || deregister.isError
  const error = updateState.error ?? deregister.error
  const isPending = updateState.isPending || deregister.isPending

  return (
    <PanelSection title={`Container instances (${instances.length})`}>
      <div className="flex flex-col items-start gap-3">
        {isError && (
          <ErrorBanner
            error={error ?? undefined}
            msg="Container instance action failed."
          />
        )}
        {instances.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No container instances found.
          </p>
        ) : (
          <div className="w-full">
            <PanelList>
              {instances.map((instance: ContainerInstance) => {
                const arn = instance.containerInstanceArn ?? ""
                const draining = instance.status === "DRAINING"
                const ec2Id = instance.ec2InstanceId

                return (
                  <PanelListItem
                    actions={
                      <>
                        <Button
                          aria-label={
                            draining
                              ? "Activate container instance"
                              : "Drain container instance"
                          }
                          onClick={() => {
                            setPending({
                              arn,
                              action: draining ? "activate" : "drain",
                            })
                          }}
                          size="sm"
                          variant="outline"
                        >
                          {draining ? "Activate" : "Drain"}
                        </Button>
                        <Button
                          onClick={() => {
                            setPending({ arn, action: "deregister" })
                          }}
                          size="sm"
                          variant="destructive"
                        >
                          Deregister
                        </Button>
                      </>
                    }
                    detail={`${instance.runningTasksCount ?? 0} running, ${instance.pendingTasksCount ?? 0} pending`}
                    key={arn}
                    state={<StateBadge state={instance.status} />}
                    title={
                      ec2Id ? (
                        <Link
                          className="font-mono text-xs text-primary hover:underline"
                          params={{ id: ec2Id }}
                          to="/ec2/describe-instances/$id"
                        >
                          {ec2Id}
                        </Link>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )
                    }
                  />
                )
              })}
            </PanelList>
          </div>
        )}
        <Button
          onClick={() => {
            setShowProvision(true)
          }}
          size="sm"
          variant="outline"
        >
          <Server data-icon="inline-start" />
          Provision EC2 capacity
        </Button>
      </div>

      <ProvisionCapacityDialog
        clusterName={clusterName}
        onOpenChange={setShowProvision}
        open={showProvision}
      />

      <DeleteConfirmationDialog
        confirmLabel={pending ? ACTION_LABELS[pending.action] : "Confirm"}
        description={pending ? ACTION_DESCRIPTIONS[pending.action] : ""}
        isPending={isPending}
        onConfirm={handleConfirm}
        onOpenChange={(open) => {
          if (!open) {
            setPending(null)
          }
        }}
        open={pending !== null}
        title="Container instance"
      />
    </PanelSection>
  )
}
