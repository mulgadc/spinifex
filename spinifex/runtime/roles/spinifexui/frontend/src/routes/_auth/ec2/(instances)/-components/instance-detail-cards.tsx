import type {
  Instance,
  InstanceBlockDeviceMapping,
  InstanceTypeInfo,
} from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { ImagePlus, Settings2, Terminal } from "lucide-react"

import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
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
import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { formatDateTime } from "@/lib/utils"

interface InstanceHeaderActionsProps {
  state: string | undefined
  consolePending: boolean
  onViewConsole: () => void
  onCreateImage: () => void
  onModify: () => void
}

export function InstanceHeaderActions({
  state,
  consolePending,
  onViewConsole,
  onCreateImage,
  onModify,
}: InstanceHeaderActionsProps) {
  const isRunning = state === "running"
  const isStopped = state === "stopped"

  return (
    <div className="flex items-center gap-2">
      {isRunning && (
        <Button
          disabled={consolePending}
          onClick={onViewConsole}
          size="sm"
          variant="outline"
        >
          <Terminal className="size-4" />
          {consolePending ? "Fetching…" : "View Console"}
        </Button>
      )}
      {(isRunning || isStopped) && (
        <Button onClick={onCreateImage} size="sm" variant="outline">
          <ImagePlus className="size-4" />
          Create Image
        </Button>
      )}
      {isStopped && (
        <Button onClick={onModify} size="sm" variant="outline">
          <Settings2 className="size-4" />
          Modify Instance
        </Button>
      )}
      <StateBadge state={state} />
    </div>
  )
}

interface ConsoleOutputCardProps {
  output: string
  onHide: () => void
}

export function ConsoleOutputCard({ output, onHide }: ConsoleOutputCardProps) {
  return (
    <DetailCard>
      <DetailCard.Header>
        <div className="flex items-center justify-between">
          Console Output
          <Button onClick={onHide} size="sm" variant="ghost">
            Hide
          </Button>
        </div>
      </DetailCard.Header>
      <div className="p-4">
        <pre className="max-h-96 overflow-y-auto rounded bg-muted p-3 font-mono text-xs whitespace-pre-wrap">
          {output}
        </pre>
      </div>
    </DetailCard>
  )
}

export function InstanceInfoCard({ instance }: { instance: Instance }) {
  return (
    <DetailCard>
      <DetailCard.Header>Instance Information</DetailCard.Header>
      <DetailCard.Content>
        <DetailRow label="Instance ID" value={instance.InstanceId} />
        <DetailRow label="Instance Type" value={instance.InstanceType} />
        <DetailRow label="Instance State" value={instance.State?.Name} />
        <DetailRow
          label="State Code"
          value={instance.State?.Code?.toString()}
        />
        <DetailRow
          label="Launch Time"
          value={formatDateTime(instance.LaunchTime)}
        />
        <DetailRow
          label="Availability Zone"
          value={instance.Placement?.AvailabilityZone}
        />
        <DetailRow
          label="Placement Group"
          value={
            instance.Placement?.GroupName ? (
              <Link
                className="text-primary hover:underline"
                to="/ec2/describe-placement-groups"
              >
                {instance.Placement.GroupName}
              </Link>
            ) : undefined
          }
        />
      </DetailCard.Content>
    </DetailCard>
  )
}

export function NetworkSecurityCard({ instance }: { instance: Instance }) {
  const hasNetworkDetails =
    instance.PrivateIpAddress ||
    instance.PublicIpAddress ||
    instance.VpcId ||
    instance.SubnetId ||
    (instance.SecurityGroups && instance.SecurityGroups.length > 0)

  if (!hasNetworkDetails) {
    return null
  }

  return (
    <DetailCard>
      <DetailCard.Header>Network & Security</DetailCard.Header>
      <DetailCard.Content>
        <DetailRow
          label="Private IP Address"
          value={instance.PrivateIpAddress}
        />
        <DetailRow label="Public IP Address" value={instance.PublicIpAddress} />
        <DetailRow label="Private DNS" value={instance.PrivateDnsName} />
        <DetailRow label="Public DNS" value={instance.PublicDnsName} />
        <DetailRow
          label="VPC ID"
          value={
            instance.VpcId ? (
              <Link
                className="text-primary hover:underline"
                params={{ id: instance.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {instance.VpcId}
              </Link>
            ) : undefined
          }
        />
        <DetailRow
          label="Subnet ID"
          value={
            instance.SubnetId ? (
              <Link
                className="text-primary hover:underline"
                params={{ id: instance.SubnetId }}
                to="/ec2/describe-subnets/$id"
              >
                {instance.SubnetId}
              </Link>
            ) : undefined
          }
        />
        <DetailRow label="Key Name" value={instance.KeyName} />
        <DetailRow
          label="Security Groups"
          value={instance.SecurityGroups?.map((sg) => sg.GroupName).join(", ")}
        />
      </DetailCard.Content>
    </DetailCard>
  )
}

export function BlockDeviceMappingsCard({
  mappings,
}: {
  mappings: InstanceBlockDeviceMapping[] | undefined
}) {
  if (!mappings || mappings.length === 0) {
    return null
  }

  return (
    <DetailCard>
      <DetailCard.Header>Block Device Mappings</DetailCard.Header>
      {mappings.map((bdm) => (
        <DetailCard.Content key={bdm.DeviceName}>
          <DetailRow label="Device Name" value={bdm.DeviceName} />
          <DetailRow
            label="Volume ID"
            value={
              bdm.Ebs?.VolumeId ? (
                <Link
                  className="text-primary hover:underline"
                  params={{ id: bdm.Ebs.VolumeId }}
                  to="/ec2/describe-volumes/$id"
                >
                  {bdm.Ebs.VolumeId}
                </Link>
              ) : undefined
            }
          />
          <DetailRow label="Status" value={bdm.Ebs?.Status} />
          <DetailRow
            label="Attach Time"
            value={formatDateTime(bdm.Ebs?.AttachTime)}
          />
          <DetailRow
            label="Delete on Termination"
            value={bdm.Ebs?.DeleteOnTermination ? "Yes" : "No"}
          />
        </DetailCard.Content>
      ))}
    </DetailCard>
  )
}

interface ModifyInstanceTypeDialogProps {
  instanceId: string
  currentType: string | undefined
  instanceTypes: InstanceTypeInfo[]
  open: boolean
  selectedType: string
  isPending: boolean
  onSelectedTypeChange: (type: string) => void
  onClose: () => void
  onSave: () => void
}

export function ModifyInstanceTypeDialog({
  instanceId,
  currentType,
  instanceTypes,
  open,
  selectedType,
  isPending,
  onSelectedTypeChange,
  onClose,
  onSave,
}: ModifyInstanceTypeDialogProps) {
  const canSave = !isPending && !!selectedType && selectedType !== currentType

  return (
    <AlertDialog
      onOpenChange={(nextOpen) => {
        if (!nextOpen) {
          onClose()
        }
      }}
      open={open}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Modify Instance</AlertDialogTitle>
          <AlertDialogDescription>
            Select a new instance type for &quot;{instanceId}&quot;. The
            instance must be stopped to modify its attributes.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <Field>
          <FieldTitle>
            <label htmlFor="instanceType">Instance Type</label>
          </FieldTitle>
          <GpuInstanceTypeSelect
            className="w-full"
            id="instanceType"
            instanceTypes={instanceTypes}
            onValueChange={onSelectedTypeChange}
            value={selectedType}
          />
        </Field>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction disabled={!canSave} onClick={onSave}>
            {isPending ? "Saving…" : "Save Changes"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
