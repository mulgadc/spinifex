import type { Cluster } from "@aws-sdk/client-eks"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  AlertTriangle,
  Eye,
  Maximize2,
  SlidersHorizontal,
  Trash2,
} from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import { CliCommandPanel } from "@/components/cli-command-panel"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
  PanelTags,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { StateBadge } from "@/components/state-badge"
import { Badge } from "@/components/ui/badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { getRegion } from "@/lib/cluster-config"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteCluster } from "@/mutations/eks"
import { eksClusterQueryOptions } from "@/queries/eks"

import { AccessTab } from "./access-tab"
import { AddonsTab } from "./addons-tab"
import { NetworkingTab } from "./networking-tab"
import { NodegroupsTab } from "./nodegroups-tab"

function kubeconfigCommands(clusterName: string) {
  return [
    {
      label: "Configure kubectl",
      parts: [
        { type: "bin" as const, value: "aws eks update-kubeconfig" },
        { type: "flag" as const, value: " --name " },
        { type: "value" as const, value: clusterName },
        { type: "flag" as const, value: " --region " },
        { type: "value" as const, value: getRegion() },
      ],
    },
    {
      label: "Verify access",
      parts: [{ type: "bin" as const, value: "kubectl get nodes" }],
    },
  ]
}

function Overview({
  clusterName,
  cluster,
}: {
  clusterName: string
  cluster: Cluster
}) {
  const healthIssues = cluster.health?.issues ?? []

  return (
    <>
      <PanelSection title="Cluster">
        <PanelFields>
          <PanelField copy={cluster.arn} label="ARN" mono>
            {cluster.arn}
          </PanelField>
          <PanelField label="Status">{cluster.status}</PanelField>
          <PanelField label="Health">
            {healthIssues.length > 0
              ? healthIssues
                  .map((issue) => issue.message)
                  .filter(Boolean)
                  .join("; ")
              : "OK"}
          </PanelField>
          <PanelField label="Kubernetes version">{cluster.version}</PanelField>
          <PanelField label="Platform version">
            {cluster.platformVersion}
          </PanelField>
          <PanelField
            copy={cluster.identity?.oidc?.issuer}
            label="OIDC issuer"
            mono
          >
            {cluster.identity?.oidc?.issuer}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(cluster.createdAt)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <NodegroupsTab
          clusterName={clusterName}
          clusterVersion={cluster.version}
          vpcId={cluster.resourcesVpcConfig?.vpcId}
        />
      </Suspense>
      <NetworkingTab cluster={cluster} />
      <PanelSection title="Connect">
        <CliCommandPanel commands={kubeconfigCommands(clusterName)} />
      </PanelSection>
    </>
  )
}

function Settings({
  clusterName,
  cluster,
  onDeleted,
}: {
  clusterName: string
  cluster: Cluster
  onDeleted: () => void
}) {
  const deleteCluster = useDeleteCluster()
  const [confirm, setConfirm] = useState(false)

  const tags = Object.entries(cluster.tags ?? {}).map(([Key, Value]) => ({
    Key,
    Value,
  }))

  return (
    <>
      <Suspense fallback={<PanelSpinner />}>
        <AddonsTab clusterName={clusterName} />
      </Suspense>
      <Suspense fallback={<PanelSpinner />}>
        <AccessTab clusterName={clusterName} />
      </Suspense>
      <PanelTags tags={tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {deleteCluster.isError && (
            <ErrorBanner
              error={deleteCluster.error}
              msg="Failed to delete cluster."
            />
          )}
          <Button
            disabled={deleteCluster.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete cluster
          </Button>
        </div>
      </PanelSection>
      <DeleteConfirmationDialog
        description={`This permanently deletes cluster "${clusterName}" and its control plane. This cannot be undone.`}
        isPending={deleteCluster.isPending}
        onConfirm={() => {
          deleteCluster.mutate(clusterName, {
            onSuccess: () => {
              setConfirm(false)
              onDeleted()
            },
          })
        }}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete cluster"
      />
    </>
  )
}

function Summary({ cluster }: { cluster: Cluster | undefined }) {
  const issues = cluster?.health?.issues?.length ?? 0

  return (
    <div className="flex flex-wrap items-center justify-center gap-2">
      <StateBadge state={cluster?.status ?? "UNKNOWN"} />
      {issues > 0 && (
        <Badge variant="destructive">
          <AlertTriangle />
          {issues === 1 ? "1 health issue" : `${issues} health issues`}
        </Badge>
      )}
    </div>
  )
}

interface ClusterDetailsProps {
  clusterName: string
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One EKS cluster's details, as the list's side panel or as its own page.
export function ClusterDetails({
  clusterName,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: ClusterDetailsProps) {
  const { data, error } = useQuery(eksClusterQueryOptions(clusterName))
  const cluster = data?.cluster

  let overview: ReactNode = <PanelSpinner />
  let settings: ReactNode = <PanelSpinner />

  if (error) {
    overview = <ErrorBanner error={error} msg="Failed to load cluster." />
    settings = overview
  } else if (cluster) {
    overview = <Overview cluster={cluster} clusterName={clusterName} />
    settings = (
      <Settings
        cluster={cluster}
        clusterName={clusterName}
        onDeleted={onClose}
      />
    )
  }

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to clusters" : "Close cluster details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open cluster page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ clusterName }}
            to="/eks/list-clusters/$clusterName"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Cluster details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      summary={<Summary cluster={cluster} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: overview,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: settings,
        },
      ]}
      title={clusterName}
    />
  )
}
