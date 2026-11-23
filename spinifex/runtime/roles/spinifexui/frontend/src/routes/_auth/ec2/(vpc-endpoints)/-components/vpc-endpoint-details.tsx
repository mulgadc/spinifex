import type { VpcEndpoint } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  Eye,
  Maximize2,
  Pencil,
  Route,
  SlidersHorizontal,
  Trash2,
} from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
import { StateBadge } from "@/components/state-badge"
import { useTopologyTab } from "@/components/topology/resource-topology"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime, getNameTag } from "@/lib/utils"
import { FULL_ACCESS_POLICY, isFullAccessPolicy } from "@/lib/vpc-endpoints"
import { useDeleteVpcEndpoint } from "@/mutations/ec2"
import {
  ec2GatewayEndpointServicesQueryOptions,
  ec2ServicePrefixListQueryOptions,
} from "@/queries/ec2"
import { PolicyDocumentViewer } from "@/routes/_auth/iam/-components/policy-document-viewer"

import { EditPolicyForm } from "./edit-policy-form"
import { ManageRouteTablesForm } from "./manage-route-tables-form"

type OpenForm = "routeTables" | "policy" | undefined

function RouteTables({
  endpoint,
  onManage,
}: {
  endpoint: VpcEndpoint
  onManage: () => void
}) {
  const tables = endpoint.RouteTableIds ?? []

  return (
    <PanelSection title="Route tables">
      <div className="flex flex-col gap-3">
        {tables.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No route tables. Subnets reach the service through this endpoint
            only once their route table is chosen.
          </p>
        ) : (
          <ul className="flex flex-col gap-1">
            {tables.map((id) => (
              <li key={id}>
                <Link
                  className="font-mono text-xs text-primary hover:underline"
                  params={{ id }}
                  to="/ec2/describe-route-tables/$id"
                >
                  {id}
                </Link>
              </li>
            ))}
          </ul>
        )}
        <div>
          <Button onClick={onManage} size="sm" variant="outline">
            <Route data-icon="inline-start" />
            Manage route tables
          </Button>
        </div>
      </div>
    </PanelSection>
  )
}

function Policy({
  endpoint,
  onEdit,
}: {
  endpoint: VpcEndpoint
  onEdit: () => void
}) {
  const { data } = useQuery(ec2GatewayEndpointServicesQueryOptions)
  const document = endpoint.PolicyDocument ?? FULL_ACCESS_POLICY
  const full = isFullAccessPolicy(endpoint.PolicyDocument)

  // A custom policy is offered only where the service says the gate enforces it.
  const editable =
    data?.ServiceDetails?.find((s) => s.ServiceName === endpoint.ServiceName)
      ?.VpcEndpointPolicySupported === true

  return (
    <PanelSection title="Policy">
      <div className="flex flex-col gap-3">
        <p className="text-sm text-muted-foreground">
          {full
            ? "Full access: any request the caller's own policies allow."
            : "Custom: only what this document allows."}
        </p>
        <PolicyDocumentViewer document={document} />
        {editable && (
          <div>
            <Button onClick={onEdit} size="sm" variant="outline">
              <Pencil data-icon="inline-start" />
              Edit policy
            </Button>
          </div>
        )}
      </div>
    </PanelSection>
  )
}

function Overview({
  endpoint,
  onOpenForm,
}: {
  endpoint: VpcEndpoint
  onOpenForm: (form: OpenForm) => void
}) {
  const service = endpoint.ServiceName ?? ""

  const { data: prefixLists } = useQuery({
    ...ec2ServicePrefixListQueryOptions(service),
    enabled: service !== "",
  })

  const prefixList = prefixLists?.PrefixLists?.[0]?.PrefixListId

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={endpoint.VpcEndpointId} label="Endpoint ID" mono>
            {endpoint.VpcEndpointId}
          </PanelField>
          <PanelField label="Status">{endpoint.State}</PanelField>
          <PanelField label="Type">{endpoint.VpcEndpointType}</PanelField>
          <PanelField copy={endpoint.ServiceName} label="Service name" mono>
            {endpoint.ServiceName}
          </PanelField>
          <PanelField copy={endpoint.VpcId} label="VPC" mono>
            {endpoint.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: endpoint.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {endpoint.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField copy={prefixList} label="Prefix list" mono>
            {prefixList}
          </PanelField>
          <PanelField label="Created">
            {endpoint.CreationTimestamp &&
              formatDateTime(endpoint.CreationTimestamp)}
          </PanelField>
          <PanelField copy={endpoint.OwnerId} label="Owner" mono>
            {endpoint.OwnerId}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <RouteTables
        endpoint={endpoint}
        onManage={() => {
          onOpenForm("routeTables")
        }}
      />
      <Policy
        endpoint={endpoint}
        onEdit={() => {
          onOpenForm("policy")
        }}
      />
    </>
  )
}

function Settings({
  endpoint,
  onDeleted,
}: {
  endpoint: VpcEndpoint
  onDeleted: () => void
}) {
  const id = endpoint.VpcEndpointId ?? ""
  const remove = useDeleteVpcEndpoint()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags
        id={endpoint.VpcEndpointId}
        invalidate={["ec2", "vpcEndpoints"]}
        tags={endpoint.Tags}
      />
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
          Delete endpoint
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? Its routes leave every route table it serves, and subnets that used them lose that path to the service. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete endpoint"
      />
    </>
  )
}

interface VpcEndpointDetailsProps {
  endpoint: VpcEndpoint
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One endpoint's details, as the list's side panel or its own page.
export function VpcEndpointDetails({
  endpoint,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: VpcEndpointDetailsProps) {
  const id = endpoint.VpcEndpointId ?? ""
  const name = getNameTag(endpoint.Tags)
  const [form, setForm] = useState<OpenForm>()

  const topology = useTopologyTab({
    page: layout === "page",
    tab,
    vpcId: endpoint.VpcId,
    focus: { kind: "vpce", id },
    noVpc: "This endpoint names no VPC.",
    pin: { vpcEndpoint: endpoint },
  })

  const closeForm = () => {
    setForm(undefined)
  }

  return (
    <>
      <DetailPanel
        closeLabel={
          layout === "page" ? "Back to endpoints" : "Close endpoint details"
        }
        fullPage={
          layout === "auto" && (
            <Link
              aria-label="Open endpoint page"
              className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
              params={{ id }}
              search={{ tab }}
              to="/ec2/describe-vpc-endpoints/$id"
            >
              <Maximize2 />
            </Link>
          )
        }
        label="Endpoint details"
        layout={layout}
        onClose={onClose}
        onTabChange={onTabChange}
        subtitle={name ? id : undefined}
        summary={
          <>
            <StateBadge state={endpoint.State} />
            {topology.toggle}
          </>
        }
        tab={tab}
        tabs={[
          {
            id: "overview",
            label: "Overview",
            icon: Eye,
            content: <Overview endpoint={endpoint} onOpenForm={setForm} />,
          },
          topology.spec,
          {
            id: "settings",
            label: "Settings",
            icon: SlidersHorizontal,
            content: <Settings endpoint={endpoint} onDeleted={onClose} />,
          },
        ]}
        title={name ?? id}
      />
      {form === "routeTables" && (
        <ManageRouteTablesForm endpoint={endpoint} onClose={closeForm} />
      )}
      {form === "policy" && (
        <EditPolicyForm endpoint={endpoint} onClose={closeForm} />
      )}
    </>
  )
}
