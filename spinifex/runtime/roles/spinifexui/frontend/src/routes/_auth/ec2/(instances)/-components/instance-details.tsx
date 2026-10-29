import type { Instance } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  ChartArea,
  Eye,
  ImagePlus,
  Maximize2,
  Pause,
  Play,
  RotateCw,
  Settings2,
  SlidersHorizontal,
  Terminal,
  Trash2,
} from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelTags,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { GpuInstanceTypeSelect } from "@/components/gpu-instance-type-select"
import { StateBadge } from "@/components/state-badge"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button, buttonVariants } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { useAdmin } from "@/hooks/use-admin"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  useGetConsoleOutput,
  useModifyInstanceAttribute,
  useRebootInstance,
  useStartInstance,
  useStopInstance,
  useTerminateInstance,
} from "@/mutations/ec2"
import { adminVMsQueryOptions } from "@/queries/admin"
import {
  ec2ImageQueryOptions,
  ec2InstanceTypesQueryOptions,
} from "@/queries/ec2"

import { CreateImageDialog } from "../../-components/create-image-dialog"
import { GpuDetailCard } from "../../-components/gpu-detail-card"
import { IamRolePanel } from "../../-components/iam-role-panel"
import { SSHAccess } from "../../-components/ssh-access"
import { InstanceMonitoring } from "./instance-monitoring"

interface InstanceDetailsProps {
  instance: Instance
  name: string | undefined
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  onClose: () => void
  // The list's side panel, or the instance's own page.
  layout?: "auto" | "page"
}

function QuickActions({ instance }: { instance: Instance }) {
  const id = instance.InstanceId ?? ""
  const state = instance.State?.Name
  const start = useStartInstance()
  const stop = useStopInstance()
  const reboot = useRebootInstance()
  const error = start.error ?? stop.error ?? reboot.error

  return (
    <>
      <div className="flex gap-2">
        <Button
          disabled={state !== "stopped" || start.isPending}
          onClick={() => {
            start.mutate(id)
          }}
          size="sm"
          variant="outline"
        >
          <Play data-icon="inline-start" />
          Start
        </Button>
        <Button
          disabled={state !== "running" || stop.isPending}
          onClick={() => {
            stop.mutate(id)
          }}
          size="sm"
          variant="outline"
        >
          <Pause data-icon="inline-start" />
          Stop
        </Button>
        <Button
          disabled={state !== "running" || reboot.isPending}
          onClick={() => {
            reboot.mutate(id)
          }}
          size="sm"
          variant="outline"
        >
          <RotateCw data-icon="inline-start" />
          Reboot
        </Button>
      </div>
      {error && <ErrorBanner error={error} />}
    </>
  )
}

function Overview({ instance }: { instance: Instance }) {
  const { isAdmin } = useAdmin()
  const { data: imageData } = useQuery(ec2ImageQueryOptions(instance.ImageId))

  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    enabled: isAdmin,
  })

  const image = imageData?.Images?.[0]

  const gpus = vmsData?.vms?.find(
    (vm) => vm.instance_id === instance.InstanceId,
  )?.gpus

  return (
    <>
      <PanelSection title="Instance information">
        <PanelFields>
          <PanelField copy={instance.InstanceId} label="Instance ID" mono>
            {instance.InstanceId}
          </PanelField>
          <PanelField label="Type" mono>
            {instance.InstanceType}
          </PanelField>
          <PanelField label="Zone">
            {instance.Placement?.AvailabilityZone}
          </PanelField>
          <PanelField label="Placement group">
            {instance.Placement?.GroupName && (
              <Link
                className="text-primary hover:underline"
                to="/ec2/describe-placement-groups"
              >
                {instance.Placement.GroupName}
              </Link>
            )}
          </PanelField>
          <PanelField label="Image" mono>
            {instance.ImageId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: instance.ImageId }}
                title={image?.Name}
                to="/ec2/describe-images/$id"
              >
                {image?.Name ?? instance.ImageId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Key pair">{instance.KeyName}</PanelField>
          <PanelField label="Launched">
            {instance.LaunchTime && formatDateTime(instance.LaunchTime)}
          </PanelField>
          {instance.State?.Name === "running" && (
            <SSHAccess image={image} instance={instance} />
          )}
        </PanelFields>
      </PanelSection>
      <PanelSection title="Networking">
        <PanelFields>
          <PanelField copy={instance.PrivateIpAddress} label="Private IP" mono>
            {instance.PrivateIpAddress}
          </PanelField>
          <PanelField copy={instance.PublicIpAddress} label="Public IP" mono>
            {instance.PublicIpAddress}
          </PanelField>
          <PanelField label="Private DNS" mono>
            {instance.PrivateDnsName}
          </PanelField>
          <PanelField label="Public DNS" mono>
            {instance.PublicDnsName}
          </PanelField>
          <PanelField label="VPC" mono>
            {instance.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: instance.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {instance.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Subnet" mono>
            {instance.SubnetId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: instance.SubnetId }}
                to="/ec2/describe-subnets/$id"
              >
                {instance.SubnetId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Security groups">
            {instance.SecurityGroups?.map((group) => group.GroupName).join(
              ", ",
            )}
          </PanelField>
        </PanelFields>
      </PanelSection>
      {(instance.BlockDeviceMappings?.length ?? 0) > 0 && (
        <PanelSection title="Storage">
          <PanelFields>
            {instance.BlockDeviceMappings?.map((device) => (
              <PanelField
                key={device.DeviceName}
                label={device.DeviceName ?? "—"}
                mono
              >
                {device.Ebs?.VolumeId && (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: device.Ebs.VolumeId }}
                    to="/ec2/describe-volumes/$id"
                  >
                    {device.Ebs.VolumeId}
                  </Link>
                )}
              </PanelField>
            ))}
          </PanelFields>
        </PanelSection>
      )}
      {instance.InstanceId && <IamRolePanel instanceId={instance.InstanceId} />}
      {isAdmin && <GpuDetailCard gpus={gpus} />}
    </>
  )
}

function ChangeTypeDialog({
  instance,
  open,
  onOpenChange,
}: {
  instance: Instance
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { data } = useQuery({
    ...ec2InstanceTypesQueryOptions,
    enabled: open,
  })

  const modify = useModifyInstanceAttribute()
  const [selected, setSelected] = useState(instance.InstanceType ?? "")

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Change instance type</AlertDialogTitle>
          <AlertDialogDescription>
            Pick a new type for {instance.InstanceId}. The instance must be
            stopped.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {modify.error && <ErrorBanner error={modify.error} />}
        <Field>
          <FieldTitle>
            <label htmlFor="instanceType">Instance type</label>
          </FieldTitle>
          <GpuInstanceTypeSelect
            className="w-full"
            id="instanceType"
            instanceTypes={data?.InstanceTypes ?? []}
            onValueChange={setSelected}
            value={selected}
          />
        </Field>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={
              modify.isPending ||
              !selected ||
              selected === instance.InstanceType
            }
            onClick={() => {
              modify.mutate(
                {
                  instanceId: instance.InstanceId ?? "",
                  instanceType: selected,
                },
                {
                  onSuccess: () => {
                    onOpenChange(false)
                  },
                },
              )
            }}
          >
            {modify.isPending ? "Saving…" : "Save"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

function Settings({
  instance,
  layout,
  onTerminated,
}: {
  instance: Instance
  layout: "auto" | "page"
  onTerminated: () => void
}) {
  const id = instance.InstanceId ?? ""
  const state = instance.State?.Name ?? ""
  const terminate = useTerminateInstance()
  const consoleOutput = useGetConsoleOutput()
  const [dialog, setDialog] = useState<"" | "terminate" | "type" | "image">("")
  const [output, setOutput] = useState<string>()

  function closeDialog(open: boolean) {
    if (!open) {
      setDialog("")
    }
  }

  return (
    <>
      <PanelTags tags={instance.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {layout === "auto" && (
            <Link
              className={buttonVariants({ size: "sm", variant: "outline" })}
              params={{ id }}
              to="/ec2/describe-instances/$id"
            >
              <Maximize2 data-icon="inline-start" />
              Open instance page
            </Link>
          )}
          <Button
            disabled={state !== "stopped"}
            onClick={() => {
              setDialog("type")
            }}
            size="sm"
            variant="outline"
          >
            <Settings2 data-icon="inline-start" />
            Change instance type
          </Button>
          <Button
            disabled={state !== "running" && state !== "stopped"}
            onClick={() => {
              setDialog("image")
            }}
            size="sm"
            variant="outline"
          >
            <ImagePlus data-icon="inline-start" />
            Create image
          </Button>
          <Button
            disabled={state !== "running" || consoleOutput.isPending}
            onClick={() => {
              consoleOutput.mutate(id, {
                onSuccess: (result) => {
                  setOutput(result.Output ? atob(result.Output) : "(no output)")
                },
              })
            }}
            size="sm"
            variant="outline"
          >
            <Terminal data-icon="inline-start" />
            {consoleOutput.isPending ? "Fetching…" : "View console output"}
          </Button>
          <Button
            disabled={
              ["terminated", "shutting-down"].includes(state) ||
              terminate.isPending
            }
            onClick={() => {
              setDialog("terminate")
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Terminate
          </Button>
          {consoleOutput.error && <ErrorBanner error={consoleOutput.error} />}
          {terminate.error && <ErrorBanner error={terminate.error} />}
        </div>
      </PanelSection>
      {output !== undefined && (
        <PanelSection title="Console output">
          <pre className="max-h-96 overflow-auto rounded bg-muted p-3 font-mono text-xs whitespace-pre-wrap">
            {output}
          </pre>
        </PanelSection>
      )}
      <ChangeTypeDialog
        instance={instance}
        key={String(dialog === "type")}
        onOpenChange={closeDialog}
        open={dialog === "type"}
      />
      <CreateImageDialog
        instanceId={id}
        onOpenChange={closeDialog}
        open={dialog === "image"}
      />
      <DeleteConfirmationDialog
        confirmLabel="Terminate"
        description={`Terminating ${id} deletes it and any volume set to delete on termination. This cannot be undone.`}
        isPending={terminate.isPending}
        onConfirm={() => {
          terminate.mutate(id, {
            onSettled: () => {
              setDialog("")
            },
            onSuccess: onTerminated,
          })
        }}
        onOpenChange={closeDialog}
        open={dialog === "terminate"}
        pendingLabel="Terminating…"
        title="Terminate instance?"
      />
    </>
  )
}

// One instance's details, as the list's side panel or as its own page. Both
// render the same tabs, so neither can fall behind the other.
export function InstanceDetails({
  instance,
  name,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: InstanceDetailsProps) {
  const id = instance.InstanceId ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to instances" : "Close instance details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open instance page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            to="/ec2/describe-instances/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Instance details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={
        <>
          <StateBadge state={instance.State?.Name} />
          <QuickActions instance={instance} />
        </>
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview instance={instance} />,
        },
        {
          id: "monitoring",
          label: "Monitoring",
          icon: ChartArea,
          content: <InstanceMonitoring instanceId={id} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: (
            <Settings
              instance={instance}
              layout={layout}
              onTerminated={onClose}
            />
          ),
        },
      ]}
      title={name ?? id}
    />
  )
}
