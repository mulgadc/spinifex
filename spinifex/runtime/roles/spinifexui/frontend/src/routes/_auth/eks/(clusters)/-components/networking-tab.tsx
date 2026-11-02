import type { Cluster } from "@aws-sdk/client-eks"
import { Link } from "@tanstack/react-router"

import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"

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
      className="font-mono text-xs text-primary underline-offset-2 hover:underline"
      params={{ id }}
      to={to}
    >
      {id}
    </Link>
  )
}

type VpcConfig = NonNullable<Cluster["resourcesVpcConfig"]>

// Endpoint access, plus the CIDRs allowed through the public endpoint.
function EndpointSection({
  cluster,
  vpc,
}: {
  cluster: Cluster | undefined
  vpc: VpcConfig | undefined
}) {
  const publicAccessCidrs = vpc?.publicAccessCidrs ?? []

  return (
    <PanelSection title="Networking">
      <PanelFields>
        <PanelField label="VPC">
          {vpc?.vpcId && (
            <ResourceLink id={vpc.vpcId} to="/ec2/describe-vpcs/$id" />
          )}
        </PanelField>
        <PanelField copy={cluster?.endpoint} label="API server endpoint" mono>
          {cluster?.endpoint}
        </PanelField>
        <PanelField label="Public access">
          {vpc?.endpointPublicAccess ? "Enabled" : "Disabled"}
        </PanelField>
        <PanelField label="Private access">
          {vpc?.endpointPrivateAccess ? "Enabled" : "Disabled"}
        </PanelField>
        {vpc?.endpointPublicAccess &&
          (publicAccessCidrs.length > 0 ? (
            publicAccessCidrs.map((cidr, index) => (
              <PanelField
                key={cidr}
                label={index === 0 ? "Public access CIDRs" : ""}
                mono
              >
                {cidr}
              </PanelField>
            ))
          ) : (
            <PanelField label="Public access CIDRs">Any (0.0.0.0/0)</PanelField>
          ))}
      </PanelFields>
    </PanelSection>
  )
}

function SubnetsSection({ subnetIds }: { subnetIds: string[] }) {
  return (
    <PanelSection title={`Subnets (${subnetIds.length})`}>
      {subnetIds.length > 0 ? (
        <ul className="flex flex-col gap-1.5 py-1">
          {subnetIds.map((id) => (
            <li key={id}>
              <ResourceLink id={id} to="/ec2/describe-subnets/$id" />
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-muted-foreground">No subnets.</p>
      )}
    </PanelSection>
  )
}

function SecurityGroupsSection({ vpc }: { vpc: VpcConfig | undefined }) {
  const securityGroupIds = vpc?.securityGroupIds ?? []

  return (
    <PanelSection title="Security groups">
      <PanelFields>
        <PanelField label="Cluster security group">
          {vpc?.clusterSecurityGroupId && (
            <ResourceLink
              id={vpc.clusterSecurityGroupId}
              to="/ec2/describe-security-groups/$id"
            />
          )}
        </PanelField>
        {securityGroupIds.length > 0 ? (
          securityGroupIds.map((id, index) => (
            <PanelField
              key={id}
              label={index === 0 ? "Additional security groups" : ""}
            >
              <ResourceLink id={id} to="/ec2/describe-security-groups/$id" />
            </PanelField>
          ))
        ) : (
          <PanelField label="Additional security groups" />
        )}
      </PanelFields>
    </PanelSection>
  )
}

// The cluster's VPC wiring: endpoint access, subnets and security groups.
export function NetworkingTab({ cluster }: { cluster: Cluster | undefined }) {
  const vpc = cluster?.resourcesVpcConfig

  return (
    <>
      <EndpointSection cluster={cluster} vpc={vpc} />
      <SubnetsSection subnetIds={vpc?.subnetIds ?? []} />
      <SecurityGroupsSection vpc={vpc} />
    </>
  )
}
