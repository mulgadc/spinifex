import type { Subnet } from "@aws-sdk/client-ec2"
import type { LoadBalancer } from "@aws-sdk/client-elastic-load-balancing-v2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  Eye,
  ListChecks,
  Maximize2,
  SlidersHorizontal,
  Trash2,
  Waypoints,
} from "lucide-react"
import { type ReactNode, Suspense, useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import {
  albAttributeSpecs,
  AttributesEditor,
} from "@/components/elbv2/attributes-editor"
import { ListenersTab } from "@/components/elbv2/listeners-tab"
import { SecurityGroupsEditor } from "@/components/elbv2/security-groups-editor"
import { SubnetsEditor } from "@/components/elbv2/subnets-editor"
import { Elbv2TagsSection } from "@/components/elbv2/tags-section"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime, getNameTag } from "@/lib/utils"
import {
  useDeleteLoadBalancer,
  useModifyLoadBalancerAttributes,
  useSetSecurityGroups,
  useSetSubnets,
} from "@/mutations/elbv2"
import {
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import { elbv2LoadBalancerAttributesQueryOptions } from "@/queries/elbv2"

function definedIds(ids: readonly (string | undefined)[] | undefined) {
  return (ids ?? []).filter((id): id is string => id !== undefined)
}

function subnetLabel(subnets: readonly Subnet[], subnetId: string): string {
  const subnet = subnets.find((s) => s.SubnetId === subnetId)
  const name = getNameTag(subnet?.Tags)
  const cidr = subnet?.CidrBlock ? ` (${subnet.CidrBlock})` : ""

  return name ? `${name}${cidr}` : `${subnetId}${cidr}`
}

function Zones({ lb }: { lb: LoadBalancer }) {
  const { data } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const subnets = data.Subnets ?? []

  return (
    <PanelFields>
      {(lb.AvailabilityZones ?? []).map((az) => (
        <PanelField
          key={`${az.ZoneName}-${az.SubnetId}`}
          label={az.ZoneName ?? "—"}
        >
          {az.SubnetId && (
            <Link
              className="text-primary hover:underline"
              params={{ id: az.SubnetId }}
              to="/ec2/describe-subnets/$id"
            >
              {subnetLabel(subnets, az.SubnetId)}
            </Link>
          )}
        </PanelField>
      ))}
    </PanelFields>
  )
}

function Overview({ lb }: { lb: LoadBalancer }) {
  const arn = lb.LoadBalancerArn ?? ""
  const securityGroups = definedIds(lb.SecurityGroups)

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Name">{lb.LoadBalancerName}</PanelField>
          <PanelField copy={arn} label="ARN" mono>
            {arn}
          </PanelField>
          <PanelField copy={lb.DNSName} label="DNS name" mono>
            {lb.DNSName}
          </PanelField>
          <PanelField label="State">
            {lb.State?.Reason
              ? `${lb.State.Code} — ${lb.State.Reason}`
              : lb.State?.Code}
          </PanelField>
          <PanelField label="Type">{lb.Type}</PanelField>
          <PanelField label="Scheme">{lb.Scheme}</PanelField>
          <PanelField label="IP address type">{lb.IpAddressType}</PanelField>
          <PanelField label="VPC" mono>
            {lb.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: lb.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {lb.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Created">
            {lb.CreatedTime && formatDateTime(lb.CreatedTime)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title="Availability zones">
        <Suspense fallback={<PanelSpinner />}>
          <Zones lb={lb} />
        </Suspense>
      </PanelSection>
      {securityGroups.length > 0 && (
        <PanelSection title="Security groups">
          <PanelFields>
            {securityGroups.map((sg) => (
              <PanelField key={sg} label="Security group" mono>
                <Link
                  className="text-primary hover:underline"
                  params={{ id: sg }}
                  to="/ec2/describe-security-groups/$id"
                >
                  {sg}
                </Link>
              </PanelField>
            ))}
          </PanelFields>
        </PanelSection>
      )}
    </>
  )
}

function Listeners({ lb }: { lb: LoadBalancer }) {
  return (
    <PanelSection title="Listeners">
      <Suspense fallback={<PanelSpinner />}>
        <ListenersTab
          lbType={lb.Type === "network" ? "network" : "application"}
          loadBalancerArn={lb.LoadBalancerArn ?? ""}
          vpcId={lb.VpcId}
        />
      </Suspense>
    </PanelSection>
  )
}

function SubnetsSection({ lb }: { lb: LoadBalancer }) {
  const arn = lb.LoadBalancerArn ?? ""
  const { data } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const setSubnets = useSetSubnets()

  return (
    <SubnetsEditor
      available={(data.Subnets ?? []).filter((s) => s.VpcId === lb.VpcId)}
      current={definedIds(lb.AvailabilityZones?.map((az) => az.SubnetId))}
      error={setSubnets.error}
      isPending={setSubnets.isPending}
      isSuccess={setSubnets.isSuccess}
      onSubmit={(subnetIds) => {
        setSubnets.mutate({ loadBalancerArn: arn, subnetIds })
      }}
    />
  )
}

function SecurityGroupsSection({ lb }: { lb: LoadBalancer }) {
  const arn = lb.LoadBalancerArn ?? ""
  const { data } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)
  const setSecurityGroups = useSetSecurityGroups()

  return (
    <SecurityGroupsEditor
      available={(data.SecurityGroups ?? []).filter(
        (sg) => sg.VpcId === lb.VpcId,
      )}
      current={definedIds(lb.SecurityGroups)}
      error={setSecurityGroups.error}
      isPending={setSecurityGroups.isPending}
      isSuccess={setSecurityGroups.isSuccess}
      onSubmit={(securityGroupIds) => {
        setSecurityGroups.mutate({ loadBalancerArn: arn, securityGroupIds })
      }}
    />
  )
}

function AttributesSection({ arn }: { arn: string }) {
  const { data } = useSuspenseQuery(
    elbv2LoadBalancerAttributesQueryOptions(arn),
  )

  const modify = useModifyLoadBalancerAttributes()

  return (
    <AttributesEditor
      attributes={data.Attributes ?? []}
      error={modify.error}
      isPending={modify.isPending}
      isSuccess={modify.isSuccess}
      onSubmit={(changed) => {
        modify.mutate({ loadBalancerArn: arn, attributes: changed })
      }}
      specs={albAttributeSpecs}
    />
  )
}

function Loading({ children }: { children: ReactNode }) {
  return <Suspense fallback={<PanelSpinner />}>{children}</Suspense>
}

function Settings({
  lb,
  onDeleted,
}: {
  lb: LoadBalancer
  onDeleted: () => void
}) {
  const arn = lb.LoadBalancerArn ?? ""
  const name = lb.LoadBalancerName ?? arn
  const remove = useDeleteLoadBalancer()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelSection title="Subnets">
        <Loading>
          <SubnetsSection lb={lb} />
        </Loading>
      </PanelSection>
      {/* NLBs carry no security groups. */}
      {lb.Type !== "network" && (
        <PanelSection title="Security groups">
          <Loading>
            <SecurityGroupsSection lb={lb} />
          </Loading>
        </PanelSection>
      )}
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
          Delete load balancer
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete load balancer "${name}"? This cannot be undone.`}
        identifier={arn}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete load balancer"
      />
    </>
  )
}

interface LoadBalancerDetailsProps {
  lb: LoadBalancer
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One load balancer's details, as the list's side panel or as its own page.
export function LoadBalancerDetails({
  lb,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: LoadBalancerDetailsProps) {
  const arn = lb.LoadBalancerArn ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to load balancers"
          : "Close load balancer details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open load balancer page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id: encodeURIComponent(arn) }}
            search={{ tab }}
            to="/ec2/describe-load-balancers/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Load balancer details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={lb.DNSName}
      summary={<StateBadge state={lb.State?.Code} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview lb={lb} />,
        },
        {
          id: "listeners",
          label: "Listeners",
          icon: Waypoints,
          content: <Listeners lb={lb} />,
        },
        {
          id: "attributes",
          label: "Attributes",
          icon: ListChecks,
          content: (
            <PanelSection title="Attributes">
              <Loading>
                <AttributesSection arn={arn} />
              </Loading>
            </PanelSection>
          ),
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings lb={lb} onDeleted={onClose} />,
        },
      ]}
      title={lb.LoadBalancerName ?? arn}
    />
  )
}
