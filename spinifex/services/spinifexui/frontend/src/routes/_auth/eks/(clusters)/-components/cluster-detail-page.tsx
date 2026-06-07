import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { CliCommandPanel } from "@/components/cli-command-panel"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { formatDateTime } from "@/lib/utils"
import { useDeleteCluster } from "@/mutations/eks"
import { eksClusterQueryOptions } from "@/queries/eks"

import { AccessTab } from "./access-tab"
import { NodegroupsTab } from "./nodegroups-tab"

const AWS_REGION = "ap-southeast-2"

function statusVariant(status: string | undefined) {
  switch (status ?? "") {
    case "ACTIVE": {
      return "default" as const
    }
    case "FAILED": {
      return "destructive" as const
    }
    default: {
      return "secondary" as const
    }
  }
}

export function ClusterDetailPage({ clusterName }: { clusterName: string }) {
  const navigate = useNavigate()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const { data: clusterData } = useSuspenseQuery(
    eksClusterQueryOptions(clusterName),
  )

  const deleteCluster = useDeleteCluster()

  const cluster = clusterData.cluster

  const kubeconfigCommands = [
    {
      label: "Configure kubectl",
      parts: [
        { type: "bin" as const, value: "aws eks update-kubeconfig" },
        { type: "flag" as const, value: " --name " },
        { type: "value" as const, value: clusterName },
        { type: "flag" as const, value: " --region " },
        { type: "value" as const, value: AWS_REGION },
      ],
    },
    {
      label: "Verify access",
      parts: [{ type: "bin" as const, value: "kubectl get nodes" }],
    },
  ]

  function handleDelete() {
    deleteCluster.mutate(clusterName, {
      onSuccess: () => {
        void navigate({ to: "/eks/list-clusters" })
      },
    })
  }

  return (
    <>
      <BackLink to="/eks/list-clusters">Clusters</BackLink>

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
            <Badge variant={statusVariant(cluster?.status)}>
              {cluster?.status ?? "UNKNOWN"}
            </Badge>
          </div>
        }
        subtitle="Cluster Details"
        title={clusterName}
      />

      {deleteCluster.isError && (
        <ErrorBanner
          error={
            deleteCluster.error instanceof Error
              ? deleteCluster.error
              : undefined
          }
          msg="Failed to delete cluster."
        />
      )}

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTab value="overview">Overview</TabsTab>
          <TabsTab value="compute">Compute</TabsTab>
          <TabsTab value="access">Access</TabsTab>
          <TabsTab value="connect">Connect</TabsTab>
        </TabsList>

        <TabsPanel value="overview">
          <DetailCard>
            <DetailCard.Header>Cluster</DetailCard.Header>
            <DetailCard.Content>
              <DetailRow label="ARN" value={cluster?.arn} />
              <DetailRow label="Status" value={cluster?.status} />
              <DetailRow label="Kubernetes version" value={cluster?.version} />
              <DetailRow
                label="Platform version"
                value={cluster?.platformVersion}
              />
              <DetailRow label="Endpoint" value={cluster?.endpoint} />
              <DetailRow
                label="OIDC issuer"
                value={cluster?.identity?.oidc?.issuer}
              />
              <DetailRow
                label="VPC"
                value={cluster?.resourcesVpcConfig?.vpcId}
              />
              <DetailRow
                label="Created at"
                value={formatDateTime(cluster?.createdAt)}
              />
            </DetailCard.Content>
          </DetailCard>
        </TabsPanel>

        <TabsPanel value="compute">
          <NodegroupsTab
            clusterName={clusterName}
            vpcId={cluster?.resourcesVpcConfig?.vpcId}
          />
        </TabsPanel>

        <TabsPanel value="access">
          <AccessTab clusterName={clusterName} />
        </TabsPanel>

        <TabsPanel value="connect">
          <CliCommandPanel commands={kubeconfigCommands} />
        </TabsPanel>
      </Tabs>

      <DeleteConfirmationDialog
        description={`This permanently deletes cluster "${clusterName}" and its control plane. This cannot be undone.`}
        isPending={deleteCluster.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete cluster"
      />
    </>
  )
}
