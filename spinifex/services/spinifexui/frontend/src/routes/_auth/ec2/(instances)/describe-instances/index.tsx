import type { Instance, Reservation } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ec2InstancesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(instances)/describe-instances/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2InstancesQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Instances | EC2 | Mulga",
      },
    ],
  }),
  component: Ec2,
})

const statePriority: Record<string, number> = {
  running: 0,
  pending: 1,
  stopping: 2,
  "shutting-down": 3,
  stopped: 4,
  terminated: 5,
}

function Ec2() {
  const { data } = useSuspenseQuery(ec2InstancesQueryOptions)

  const instances = (
    data.Reservations?.flatMap(
      (reservation: Reservation) => reservation.Instances ?? [],
    ) ?? []
  ).toSorted((a, b) => {
    const pa = statePriority[a.State?.Name ?? ""] ?? 6
    const pb = statePriority[b.State?.Name ?? ""] ?? 6
    if (pa !== pb) {
      return pa - pb
    }
    return (a.InstanceId ?? "").localeCompare(b.InstanceId ?? "")
  })

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/run-instances">
            <Button>Run Instances</Button>
          </Link>
        }
        title="Instances"
      />

      {instances.length > 0 ? (
        <div className="space-y-4">
          {instances.map((instance: Instance) => {
            if (!instance.InstanceId) {
              return null
            }
            return (
              <ListCard
                badge={<StateBadge state={instance.State?.Name} />}
                key={instance.InstanceId}
                params={{ id: instance.InstanceId }}
                subtitle={`${instance.InstanceType}`}
                title={instance.InstanceId}
                to="/ec2/describe-instances/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No instances found.</p>
      )}
    </>
  )
}
