import type { Cluster } from "@aws-sdk/client-eks"
import { Link } from "@tanstack/react-router"

import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"

function ResourceLink({
  to,
  id,
}: {
  to:
    | "/ec2/describe-vpcs/$id"
    | "/ec2/describe-subnets/$id"
    | "/ec2/describe-security-groups/$id"
  id: string
}) {
  return (
    <Link
      className="font-mono text-primary underline-offset-2 hover:underline"
      params={{ id }}
      to={to}
    >
      {id}
    </Link>
  )
}

export function NetworkingTab({ cluster }: { cluster: Cluster | undefined }) {
  const vpc = cluster?.resourcesVpcConfig
  const subnetIds = vpc?.subnetIds ?? []
  const securityGroupIds = vpc?.securityGroupIds ?? []
  const publicAccessCidrs = vpc?.publicAccessCidrs ?? []

  return (
    <>
      <DetailCard>
        <DetailCard.Header>VPC</DetailCard.Header>
        <DetailCard.Content>
          <DetailRow
            label="VPC"
            value={
              vpc?.vpcId ? (
                <ResourceLink id={vpc.vpcId} to="/ec2/describe-vpcs/$id" />
              ) : undefined
            }
          />
          <DetailRow label="API server endpoint" value={cluster?.endpoint} />
          <DetailRow
            label="Public access"
            value={vpc?.endpointPublicAccess ? "Enabled" : "Disabled"}
          />
          <DetailRow
            label="Private access"
            value={vpc?.endpointPrivateAccess ? "Enabled" : "Disabled"}
          />
          {vpc?.endpointPublicAccess && (
            <DetailRow
              label="Public access CIDRs"
              value={
                publicAccessCidrs.length > 0 ? (
                  <div className="space-y-1 font-mono text-xs">
                    {publicAccessCidrs.map((cidr) => (
                      <div key={cidr}>{cidr}</div>
                    ))}
                  </div>
                ) : (
                  "Any (0.0.0.0/0)"
                )
              }
            />
          )}
        </DetailCard.Content>
      </DetailCard>

      <DetailCard>
        <DetailCard.Header>Subnets</DetailCard.Header>
        <DetailCard.Content>
          {subnetIds.length > 0 ? (
            <div className="space-y-1">
              {subnetIds.map((id) => (
                <ResourceLink id={id} key={id} to="/ec2/describe-subnets/$id" />
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground">No subnets.</p>
          )}
        </DetailCard.Content>
      </DetailCard>

      <DetailCard>
        <DetailCard.Header>Security groups</DetailCard.Header>
        <DetailCard.Content>
          <DetailRow
            label="Cluster security group"
            value={
              vpc?.clusterSecurityGroupId ? (
                <ResourceLink
                  id={vpc.clusterSecurityGroupId}
                  to="/ec2/describe-security-groups/$id"
                />
              ) : undefined
            }
          />
          <DetailRow
            label="Additional security groups"
            value={
              securityGroupIds.length > 0 ? (
                <div className="space-y-1">
                  {securityGroupIds.map((id) => (
                    <ResourceLink
                      id={id}
                      key={id}
                      to="/ec2/describe-security-groups/$id"
                    />
                  ))}
                </div>
              ) : (
                "—"
              )
            }
          />
        </DetailCard.Content>
      </DetailCard>
    </>
  )
}
