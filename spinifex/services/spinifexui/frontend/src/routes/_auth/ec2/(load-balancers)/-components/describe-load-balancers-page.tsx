import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"

import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { elbv2LoadBalancersQueryOptions } from "@/queries/elbv2"

function formatCreatedAt(createdAt: Date | undefined): string {
  if (!createdAt) {
    return ""
  }
  return new Date(createdAt).toLocaleString()
}

export function DescribeLoadBalancersPage() {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(elbv2LoadBalancersQueryOptions)

  const loadBalancers = data.LoadBalancers ?? []

  return (
    <>
      <PageHeading
        actions={
          <Button
            onClick={async () =>
              await navigate({ to: "/ec2/create-load-balancer" })
            }
          >
            Create Load Balancer
          </Button>
        }
        title="Load Balancers"
      />

      {loadBalancers.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">DNS Name</th>
                <th className="px-4 py-2 font-medium">State</th>
                <th className="px-4 py-2 font-medium">Type</th>
                <th className="px-4 py-2 font-medium">Scheme</th>
                <th className="px-4 py-2 font-medium">VPC</th>
                <th className="px-4 py-2 font-medium">Created</th>
              </tr>
            </thead>
            <tbody>
              {loadBalancers.map((lb: LoadBalancer) => {
                if (!lb.LoadBalancerArn) {
                  return null
                }
                const arn = lb.LoadBalancerArn
                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={arn}
                    onClick={async () =>
                      await navigate({
                        to: "/ec2/describe-load-balancers/$id",
                        params: { id: encodeURIComponent(arn) },
                      })
                    }
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                        params={{ id: encodeURIComponent(arn) }}
                        to="/ec2/describe-load-balancers/$id"
                      >
                        {lb.LoadBalancerName ?? arn}
                      </Link>
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {lb.DNSName ?? ""}
                    </td>
                    <td className="px-4 py-2">
                      <StateBadge state={lb.State?.Code} />
                    </td>
                    <td className="px-4 py-2">{lb.Type ?? ""}</td>
                    <td className="px-4 py-2">{lb.Scheme ?? ""}</td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {lb.VpcId ?? ""}
                    </td>
                    <td className="px-4 py-2 text-xs">
                      {formatCreatedAt(lb.CreatedTime)}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-muted-foreground">No load balancers found.</p>
      )}
    </>
  )
}
