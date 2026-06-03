import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import {
  AttributesEditor,
  targetGroupAttributeSpecs,
} from "@/components/elbv2/attributes-editor"
import { TagsEditor } from "@/components/elbv2/tags-editor"
import { TargetsTab } from "@/components/elbv2/targets-tab"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import {
  useDeleteTargetGroup,
  useModifyTargetGroupAttributes,
  useUpdateTags,
} from "@/mutations/elbv2"
import {
  elbv2TagsQueryOptions,
  elbv2TargetGroupAttributesQueryOptions,
  elbv2TargetGroupQueryOptions,
} from "@/queries/elbv2"

interface Props {
  arn: string
}

export function TargetGroupDetailPage({ arn }: Props) {
  const navigate = useNavigate()
  const { data: tgData } = useSuspenseQuery(elbv2TargetGroupQueryOptions(arn))
  const { data: attrsData } = useSuspenseQuery(
    elbv2TargetGroupAttributesQueryOptions(arn),
  )
  const { data: tagsData } = useSuspenseQuery(elbv2TagsQueryOptions([arn]))

  const deleteMutation = useDeleteTargetGroup()
  const modifyAttrsMutation = useModifyTargetGroupAttributes()
  const updateTagsMutation = useUpdateTags()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [activeTab, setActiveTab] = useState("overview")

  const tg = tgData.TargetGroups?.[0]

  if (!tg?.TargetGroupArn) {
    return (
      <>
        <BackLink to="/ec2/describe-target-groups">
          Back to target groups
        </BackLink>
        <p className="text-muted-foreground">Target group not found.</p>
      </>
    )
  }

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(arn)
      navigate({ to: "/ec2/describe-target-groups" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  const attributes = attrsData.Attributes ?? []
  const tgTags =
    tagsData?.TagDescriptions?.find((td) => td.ResourceArn === arn)?.Tags ?? []

  return (
    <>
      <BackLink to="/ec2/describe-target-groups">
        Back to target groups
      </BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete target group"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <Button
              onClick={() => setShowDeleteDialog(true)}
              size="sm"
              variant="destructive"
            >
              <Trash2 className="size-4" />
              Delete
            </Button>
          }
          subtitle="Target Group Details"
          title={tg.TargetGroupName ?? tg.TargetGroupArn}
        />

        <Tabs onValueChange={setActiveTab} value={activeTab}>
          <TabsList>
            <TabsTab value="overview">Overview</TabsTab>
            <TabsTab value="targets">Targets</TabsTab>
            <TabsTab value="health-checks">Health checks</TabsTab>
            <TabsTab value="attributes">Attributes</TabsTab>
            <TabsTab value="tags">Tags</TabsTab>
          </TabsList>

          <TabsPanel value="overview">
            <DetailCard>
              <DetailCard.Header>Target group</DetailCard.Header>
              <DetailCard.Content>
                <DetailRow label="ARN" value={tg.TargetGroupArn} />
                <DetailRow label="Protocol" value={tg.Protocol} />
                <DetailRow label="Port" value={tg.Port?.toString()} />
                <DetailRow label="VPC" value={tg.VpcId} />
                <DetailRow label="Target type" value={tg.TargetType} />
                <DetailRow
                  label="Protocol version"
                  value={tg.ProtocolVersion}
                />
                <DetailRow label="IP address type" value={tg.IpAddressType} />
              </DetailCard.Content>
            </DetailCard>
          </TabsPanel>

          <TabsPanel value="targets">
            <TargetsTab
              defaultPort={tg.Port ?? 80}
              isActive={activeTab === "targets"}
              targetGroupArn={tg.TargetGroupArn}
              vpcId={tg.VpcId}
            />
          </TabsPanel>

          <TabsPanel value="health-checks">
            <p className="mb-3 text-sm text-muted-foreground">
              Health-check settings are set at creation time. Editing lands in a
              future slice (mulga-948).
            </p>
            <DetailCard>
              <DetailCard.Header>Health check</DetailCard.Header>
              <DetailCard.Content>
                <DetailRow
                  label="Enabled"
                  value={
                    tg.HealthCheckEnabled === undefined
                      ? undefined
                      : String(tg.HealthCheckEnabled)
                  }
                />
                <DetailRow label="Protocol" value={tg.HealthCheckProtocol} />
                <DetailRow label="Path" value={tg.HealthCheckPath} />
                <DetailRow label="Port" value={tg.HealthCheckPort} />
                <DetailRow
                  label="Interval (s)"
                  value={tg.HealthCheckIntervalSeconds?.toString()}
                />
                <DetailRow
                  label="Timeout (s)"
                  value={tg.HealthCheckTimeoutSeconds?.toString()}
                />
                <DetailRow
                  label="Healthy threshold"
                  value={tg.HealthyThresholdCount?.toString()}
                />
                <DetailRow
                  label="Unhealthy threshold"
                  value={tg.UnhealthyThresholdCount?.toString()}
                />
                <DetailRow
                  label="Matcher (HTTP codes)"
                  value={tg.Matcher?.HttpCode ?? tg.Matcher?.GrpcCode}
                />
              </DetailCard.Content>
            </DetailCard>
          </TabsPanel>

          <TabsPanel value="attributes">
            <AttributesEditor
              attributes={attributes}
              error={modifyAttrsMutation.error}
              isPending={modifyAttrsMutation.isPending}
              isSuccess={modifyAttrsMutation.isSuccess}
              onSubmit={(changed) => {
                modifyAttrsMutation.mutate({
                  targetGroupArn: arn,
                  attributes: changed,
                })
              }}
              specs={targetGroupAttributeSpecs}
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
                  initialKeys: tgTags
                    .map((t) => t.Key ?? "")
                    .filter((k) => k.length > 0),
                })
              }
              tags={tgTags}
            />
          </TabsPanel>
        </Tabs>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete target group "${
          tg.TargetGroupName ?? tg.TargetGroupArn
        }"? This action cannot be undone.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Target Group"
      />
    </>
  )
}
