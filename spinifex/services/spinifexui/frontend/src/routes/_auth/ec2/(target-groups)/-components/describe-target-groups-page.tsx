import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"

import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import {
  elbv2TargetGroupsQueryOptions,
  elbv2TargetHealthQueryOptions,
} from "@/queries/elbv2"

export function DescribeTargetGroupsPage() {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(elbv2TargetGroupsQueryOptions)

  const targetGroups = data.TargetGroups ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-target-group">
            <Button>Create Target Group</Button>
          </Link>
        }
        title="Target Groups"
      />

      {targetGroups.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Protocol</th>
                <th className="px-4 py-2 font-medium">Port</th>
                <th className="px-4 py-2 font-medium">VPC</th>
                <th className="px-4 py-2 font-medium">Target Type</th>
                <th className="px-4 py-2 font-medium">Targets</th>
                <th className="px-4 py-2 font-medium">Health</th>
              </tr>
            </thead>
            <tbody>
              {targetGroups.map((tg: TargetGroup) => {
                if (!tg.TargetGroupArn) {
                  return null
                }
                const arn = tg.TargetGroupArn
                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={arn}
                    onClick={async () =>
                      await navigate({
                        to: "/ec2/describe-target-groups/$id",
                        params: { id: encodeURIComponent(arn) },
                      })
                    }
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                        params={{ id: encodeURIComponent(arn) }}
                        to="/ec2/describe-target-groups/$id"
                      >
                        {tg.TargetGroupName ?? arn}
                      </Link>
                    </td>
                    <td className="px-4 py-2">{tg.Protocol ?? ""}</td>
                    <td className="px-4 py-2">{tg.Port ?? ""}</td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {tg.VpcId ?? ""}
                    </td>
                    <td className="px-4 py-2">{tg.TargetType ?? ""}</td>
                    <TargetHealthSummaryCells arn={arn} />
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-muted-foreground">No target groups found.</p>
      )}
    </>
  )
}

function TargetHealthSummaryCells({ arn }: { arn: string }) {
  const { data, isLoading } = useQuery(elbv2TargetHealthQueryOptions(arn))
  if (isLoading) {
    return (
      <>
        <td className="px-4 py-2 text-muted-foreground">…</td>
        <td className="px-4 py-2 text-muted-foreground">…</td>
      </>
    )
  }
  const descriptions = data?.TargetHealthDescriptions ?? []
  const counts: Record<string, number> = {}
  for (const desc of descriptions) {
    const state = desc.TargetHealth?.State ?? "unknown"
    counts[state] = (counts[state] ?? 0) + 1
  }
  const summary =
    Object.entries(counts)
      .map(([state, count]) => `${count} ${state}`)
      .join(", ") || "—"
  return (
    <>
      <td className="px-4 py-2">{descriptions.length}</td>
      <td className="px-4 py-2 text-xs">{summary}</td>
    </>
  )
}
