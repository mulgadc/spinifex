import type { Instance } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import {
  ChartArea,
  Eye,
  Maximize2,
  Pause,
  Play,
  RotateCw,
  SlidersHorizontal,
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
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  useRebootInstance,
  useStartInstance,
  useStopInstance,
  useTerminateInstance,
} from "@/mutations/ec2"

import { InstanceMonitoring } from "./instance-monitoring"

interface InstancePanelProps {
  instance: Instance
  name: string | undefined
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  onClose: () => void
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
  return (
    <>
      <PanelSection title="Details">
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
          <PanelField label="Image" mono>
            {instance.ImageId}
          </PanelField>
          <PanelField label="Key pair">{instance.KeyName}</PanelField>
          <PanelField label="Launched">
            {instance.LaunchTime && formatDateTime(instance.LaunchTime)}
          </PanelField>
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
    </>
  )
}

function Settings({
  instance,
  onTerminated,
}: {
  instance: Instance
  onTerminated: () => void
}) {
  const id = instance.InstanceId ?? ""
  const state = instance.State?.Name ?? ""
  const terminate = useTerminateInstance()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelTags tags={instance.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <p className="text-sm text-muted-foreground">
            Changing the instance type, reading the console and creating an
            image are on the instance&apos;s own page.
          </p>
          <Link
            className={buttonVariants({ size: "sm", variant: "outline" })}
            params={{ id }}
            to="/ec2/describe-instances/$id"
          >
            <Maximize2 data-icon="inline-start" />
            Open instance page
          </Link>
          <Button
            disabled={
              ["terminated", "shutting-down"].includes(state) ||
              terminate.isPending
            }
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Terminate
          </Button>
          {terminate.error && <ErrorBanner error={terminate.error} />}
        </div>
      </PanelSection>
      <DeleteConfirmationDialog
        confirmLabel="Terminate"
        description={`Terminating ${id} deletes it and any volume set to delete on termination. This cannot be undone.`}
        isPending={terminate.isPending}
        onConfirm={() => {
          terminate.mutate(id, {
            onSettled: () => {
              setConfirm(false)
            },
            onSuccess: onTerminated,
          })
        }}
        onOpenChange={setConfirm}
        open={confirm}
        pendingLabel="Terminating…"
        title="Terminate instance?"
      />
    </>
  )
}

export function InstancePanel({
  instance,
  name,
  tab,
  onTabChange,
  onClose,
}: InstancePanelProps) {
  const id = instance.InstanceId ?? ""

  return (
    <DetailPanel
      closeLabel="Close instance details"
      fullPage={
        <Link
          aria-label="Open instance page"
          className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
          params={{ id }}
          to="/ec2/describe-instances/$id"
        >
          <Maximize2 />
        </Link>
      }
      label="Instance details"
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
          content: <Settings instance={instance} onTerminated={onClose} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
