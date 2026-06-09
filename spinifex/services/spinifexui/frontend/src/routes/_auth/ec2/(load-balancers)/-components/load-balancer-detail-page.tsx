import type { Subnet } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import {
  albAttributeSpecs,
  AttributesEditor,
} from "@/components/elbv2/attributes-editor"
import { ListenersTab } from "@/components/elbv2/listeners-tab"
import { SecurityGroupsEditor } from "@/components/elbv2/security-groups-editor"
import { SubnetsEditor } from "@/components/elbv2/subnets-editor"
import { TagsEditor } from "@/components/elbv2/tags-editor"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { formatDateTime, getNameTag } from "@/lib/utils"
import {
  useDeleteLoadBalancer,
  useModifyLoadBalancerAttributes,
  useSetSecurityGroups,
  useSetSubnets,
  useUpdateTags,
} from "@/mutations/elbv2"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  elbv2LoadBalancerAttributesQueryOptions,
  elbv2LoadBalancerQueryOptions,
  elbv2TagsQueryOptions,
} from "@/queries/elbv2"

interface Props {
  arn: string
}

export function LoadBalancerDetailPage({ arn }: Props) {
  const navigate = useNavigate()
  const { data: lbData } = useSuspenseQuery(elbv2LoadBalancerQueryOptions(arn))
  const { data: attrsData } = useSuspenseQuery(
    elbv2LoadBalancerAttributesQueryOptions(arn),
  )
  const { data: tagsData } = useSuspenseQuery(elbv2TagsQueryOptions([arn]))
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const { data: sgsData } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)

  const deleteMutation = useDeleteLoadBalancer()
  const modifyAttrsMutation = useModifyLoadBalancerAttributes()
  const updateTagsMutation = useUpdateTags()
  const setSecurityGroupsMutation = useSetSecurityGroups()
  const setSubnetsMutation = useSetSubnets()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const lb = lbData.LoadBalancers?.[0]

  if (!lb?.LoadBalancerArn) {
    return (
      <>
        <BackLink to="/ec2/describe-load-balancers">
          Back to load balancers
        </BackLink>
        <p className="text-muted-foreground">Load balancer not found.</p>
      </>
    )
  }

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(arn)
      navigate({ to: "/ec2/describe-load-balancers" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const subnets = subnetsData.Subnets ?? []
  const subnetLabel = (subnetId: string): string => {
    const subnet = subnets.find((s: Subnet) => s.SubnetId === subnetId)
    if (!subnet) {
      return subnetId
    }
    const name = getNameTag(subnet.Tags)
    const cidr = subnet.CidrBlock ? ` (${subnet.CidrBlock})` : ""
    return name ? `${subnetId} — ${name}${cidr}` : `${subnetId}${cidr}`
  }

  const attributes = attrsData.Attributes ?? []
  const lbTags =
    tagsData?.TagDescriptions?.find((td) => td.ResourceArn === arn)?.Tags ?? []
  const currentSecurityGroups = (lb.SecurityGroups ?? []).filter(
    (id): id is string => id !== undefined,
  )
  const vpcSecurityGroups = (sgsData.SecurityGroups ?? []).filter(
    (sg) => sg.VpcId === lb.VpcId,
  )
  const vpcSubnets = subnets.filter((s) => s.VpcId === lb.VpcId)
  const currentSubnets = (lb.AvailabilityZones ?? [])
    .map((az) => az.SubnetId)
    .filter((id): id is string => id !== undefined)
  // NLBs carry no security groups, so the editor tab is hidden for them.
  const isNlb = lb.Type === "network"

  return (
    <>
      <BackLink to="/ec2/describe-load-balancers">
        Back to load balancers
      </BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete load balancer"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                onClick={() => setShowDeleteDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
              <StateBadge state={lb.State?.Code} />
            </div>
          }
          subtitle="Load Balancer Details"
          title={lb.LoadBalancerName ?? lb.LoadBalancerArn}
        />

        <Tabs defaultValue="overview">
          <TabsList>
            <TabsTab value="overview">Overview</TabsTab>
            <TabsTab value="listeners">Listeners</TabsTab>
            <TabsTab value="subnets">Subnets</TabsTab>
            {!isNlb && (
              <TabsTab value="security-groups">Security groups</TabsTab>
            )}
            <TabsTab value="attributes">Attributes</TabsTab>
            <TabsTab value="tags">Tags</TabsTab>
          </TabsList>

          <TabsPanel value="overview">
            <DetailCard>
              <DetailCard.Header>Load balancer</DetailCard.Header>
              <DetailCard.Content>
                <DetailRow label="ARN" value={lb.LoadBalancerArn} />
                <DetailRow label="DNS name" value={lb.DNSName} />
                <DetailRow
                  label="State"
                  value={
                    lb.State?.Reason
                      ? `${lb.State.Code} — ${lb.State.Reason}`
                      : lb.State?.Code
                  }
                />
                <DetailRow label="Type" value={lb.Type} />
                <DetailRow label="Scheme" value={lb.Scheme} />
                <DetailRow label="IP address type" value={lb.IpAddressType} />
                <DetailRow label="VPC" value={lb.VpcId} />
                <DetailRow
                  label="Created at"
                  value={formatDateTime(lb.CreatedTime)}
                />
              </DetailCard.Content>
            </DetailCard>

            <DetailCard>
              <DetailCard.Header>Availability zones</DetailCard.Header>
              <DetailCard.Content>
                {(lb.AvailabilityZones ?? []).map((az) => (
                  <DetailRow
                    key={`${az.ZoneName}-${az.SubnetId}`}
                    label={az.ZoneName ?? "—"}
                    value={az.SubnetId ? subnetLabel(az.SubnetId) : undefined}
                  />
                ))}
              </DetailCard.Content>
            </DetailCard>

            {(lb.SecurityGroups ?? []).length > 0 && (
              <DetailCard>
                <DetailCard.Header>Security groups</DetailCard.Header>
                <DetailCard.Content>
                  {(lb.SecurityGroups ?? []).map((sg) => (
                    <DetailRow key={sg} label="Security group" value={sg} />
                  ))}
                </DetailCard.Content>
              </DetailCard>
            )}
          </TabsPanel>

          <TabsPanel value="listeners">
            <ListenersTab
              lbType={isNlb ? "network" : "application"}
              loadBalancerArn={arn}
              vpcId={lb.VpcId}
            />
          </TabsPanel>

          <TabsPanel value="subnets">
            <SubnetsEditor
              available={vpcSubnets}
              current={currentSubnets}
              error={setSubnetsMutation.error}
              isPending={setSubnetsMutation.isPending}
              isSuccess={setSubnetsMutation.isSuccess}
              onSubmit={(subnetIds) =>
                setSubnetsMutation.mutate({
                  loadBalancerArn: arn,
                  subnetIds,
                })
              }
            />
          </TabsPanel>

          {!isNlb && (
            <TabsPanel value="security-groups">
              <SecurityGroupsEditor
                available={vpcSecurityGroups}
                current={currentSecurityGroups}
                error={setSecurityGroupsMutation.error}
                isPending={setSecurityGroupsMutation.isPending}
                isSuccess={setSecurityGroupsMutation.isSuccess}
                onSubmit={(securityGroupIds) =>
                  setSecurityGroupsMutation.mutate({
                    loadBalancerArn: arn,
                    securityGroupIds,
                  })
                }
              />
            </TabsPanel>
          )}

          <TabsPanel value="attributes">
            <AttributesEditor
              attributes={attributes}
              error={modifyAttrsMutation.error}
              isPending={modifyAttrsMutation.isPending}
              isSuccess={modifyAttrsMutation.isSuccess}
              onSubmit={(changed) => {
                modifyAttrsMutation.mutate({
                  loadBalancerArn: arn,
                  attributes: changed,
                })
              }}
              specs={albAttributeSpecs}
            />
          </TabsPanel>

          <TabsPanel value="tags">
            <TagsEditor
              error={updateTagsMutation.error}
              isPending={updateTagsMutation.isPending}
              isSuccess={updateTagsMutation.isSuccess}
              onSubmit={(tags) =>
                updateTagsMutation.mutate({
                  resourceArn: arn,
                  tags,
                  initialKeys: lbTags
                    .map((t) => t.Key ?? "")
                    .filter((k) => k.length > 0),
                })
              }
              tags={lbTags}
            />
          </TabsPanel>
        </Tabs>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete load balancer "${
          lb.LoadBalancerName ?? lb.LoadBalancerArn
        }"? This action cannot be undone.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete load balancer"
      />
    </>
  )
}
