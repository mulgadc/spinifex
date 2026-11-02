import type { TargetGroup } from "@aws-sdk/client-elastic-load-balancing-v2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import {
  AttributesEditor,
  targetGroupAttributeSpecs,
} from "@/components/elbv2/attributes-editor"
import { Elbv2TagsSection } from "@/components/elbv2/tags-section"
import { TargetsTab } from "@/components/elbv2/targets-tab"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import {
  useDeleteTargetGroup,
  useModifyTargetGroupAttributes,
} from "@/mutations/elbv2"
import { elbv2TargetGroupAttributesQueryOptions } from "@/queries/elbv2"

function Loading({ children }: { children: ReactNode }) {
  return <Suspense fallback={<PanelSpinner />}>{children}</Suspense>
}

// An ELBv2 ARN ends ".../loadbalancer/app/<name>/<id>", so the name is second last.
function loadBalancerName(arn: string): string {
  return arn.split("/").at(-2) ?? arn
}

function Overview({ tg, active }: { tg: TargetGroup; active: boolean }) {
  const arn = tg.TargetGroupArn ?? ""
  const loadBalancers = tg.LoadBalancerArns ?? []

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Name">{tg.TargetGroupName}</PanelField>
          <PanelField copy={arn} label="ARN" mono>
            {arn}
          </PanelField>
          <PanelField label="Protocol">{tg.Protocol}</PanelField>
          <PanelField label="Port">{tg.Port}</PanelField>
          <PanelField label="VPC" mono>
            {tg.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: tg.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {tg.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Target type">{tg.TargetType}</PanelField>
          <PanelField label="Protocol version">{tg.ProtocolVersion}</PanelField>
          <PanelField label="IP address type">{tg.IpAddressType}</PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title="Load balancers">
        {loadBalancers.length ? (
          <PanelFields>
            {loadBalancers.map((lbArn) => (
              <PanelField key={lbArn} label="Load balancer">
                <Link
                  className="text-primary hover:underline"
                  params={{ id: encodeURIComponent(lbArn) }}
                  to="/ec2/describe-load-balancers/$id"
                >
                  {loadBalancerName(lbArn)}
                </Link>
              </PanelField>
            ))}
          </PanelFields>
        ) : (
          <p className="text-sm text-muted-foreground">
            No load balancer forwards to this target group.
          </p>
        )}
      </PanelSection>
      <PanelSection title="Targets">
        <Loading>
          <TargetsTab
            defaultPort={tg.Port ?? 80}
            isActive={active}
            targetGroupArn={arn}
            vpcId={tg.VpcId}
          />
        </Loading>
      </PanelSection>
      <PanelSection title="Health check">
        <PanelFields>
          <PanelField label="Enabled">
            {tg.HealthCheckEnabled === undefined
              ? undefined
              : String(tg.HealthCheckEnabled)}
          </PanelField>
          <PanelField label="Protocol">{tg.HealthCheckProtocol}</PanelField>
          <PanelField label="Path" mono>
            {tg.HealthCheckPath}
          </PanelField>
          <PanelField label="Port">{tg.HealthCheckPort}</PanelField>
          <PanelField label="Interval (s)">
            {tg.HealthCheckIntervalSeconds}
          </PanelField>
          <PanelField label="Timeout (s)">
            {tg.HealthCheckTimeoutSeconds}
          </PanelField>
          <PanelField label="Healthy threshold">
            {tg.HealthyThresholdCount}
          </PanelField>
          <PanelField label="Unhealthy threshold">
            {tg.UnhealthyThresholdCount}
          </PanelField>
          <PanelField label="Success codes">
            {tg.Matcher?.HttpCode ?? tg.Matcher?.GrpcCode}
          </PanelField>
        </PanelFields>
        <p className="pt-2 text-xs text-muted-foreground">
          Health check settings are fixed when the target group is created.
        </p>
      </PanelSection>
    </>
  )
}

function AttributesSection({ arn }: { arn: string }) {
  const { data } = useSuspenseQuery(elbv2TargetGroupAttributesQueryOptions(arn))
  const modify = useModifyTargetGroupAttributes()

  return (
    <AttributesEditor
      attributes={data.Attributes ?? []}
      error={modify.error}
      isPending={modify.isPending}
      isSuccess={modify.isSuccess}
      onSubmit={(changed) => {
        modify.mutate({ targetGroupArn: arn, attributes: changed })
      }}
      specs={targetGroupAttributeSpecs}
    />
  )
}

function Settings({
  tg,
  onDeleted,
}: {
  tg: TargetGroup
  onDeleted: () => void
}) {
  const arn = tg.TargetGroupArn ?? ""
  const remove = useDeleteTargetGroup()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelSection title="Attributes">
        <Loading>
          <AttributesSection arn={arn} />
        </Loading>
      </PanelSection>
      <PanelSection title="Tags">
        <Loading>
          <Elbv2TagsSection arn={arn} />
        </Loading>
      </PanelSection>
      <PanelSection title="Manage">
        <Button
          disabled={remove.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete target group
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete target group "${tg.TargetGroupName ?? arn}"? This cannot be undone.`}
        identifier={arn}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete target group"
      />
    </>
  )
}

interface TargetGroupDetailsProps {
  tg: TargetGroup
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One target group's details, as the list's side panel or as its own page.
export function TargetGroupDetails({
  tg,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: TargetGroupDetailsProps) {
  const arn = tg.TargetGroupArn ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to target groups"
          : "Close target group details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open target group page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id: encodeURIComponent(arn) }}
            search={{ tab }}
            to="/ec2/describe-target-groups/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Target group details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={[tg.Protocol, tg.Port].filter(Boolean).join(":") || undefined}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          // Target health polls only while it is on screen.
          content: <Overview active={tab === "overview"} tg={tg} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} tg={tg} />,
        },
      ]}
      title={tg.TargetGroupName ?? arn}
    />
  )
}
