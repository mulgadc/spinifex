import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"
import { ImagePlus, Settings2, Terminal } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useAdmin } from "@/contexts/admin-context"
import { formatDateTime, formatVRAMMiB } from "@/lib/utils"
import {
  useGetConsoleOutput,
  useModifyInstanceAttribute,
} from "@/mutations/ec2"
import { adminVMsQueryOptions } from "@/queries/admin"
import {
  ec2ImageQueryOptions,
  ec2InstanceQueryOptions,
  ec2InstanceTypesQueryOptions,
} from "@/queries/ec2"

import { AmiDetails } from "../../-components/ami-details"
import { CreateImageDialog } from "../../-components/create-image-dialog"
import { InstanceActions } from "../../-components/instance-actions"

export const Route = createFileRoute(
  "/_auth/ec2/(instances)/describe-instances/$id",
)({
  loader: async ({ context, params }) => {
    const [instanceData] = await Promise.all([
      context.queryClient.ensureQueryData(ec2InstanceQueryOptions(params.id)),
      context.queryClient.ensureQueryData(ec2InstanceTypesQueryOptions),
    ])
    const imageId = instanceData.Reservations?.[0]?.Instances?.[0]?.ImageId
    await context.queryClient.ensureQueryData(ec2ImageQueryOptions(imageId))
    return instanceData
  },
  head: ({ loaderData }) => ({
    meta: [
      {
        title: `${loaderData?.Reservations?.[0]?.Instances?.[0]?.InstanceId ?? "Instance"} | EC2 | Mulga`,
      },
    ],
  }),
  component: InstanceDetail,
})

function InstanceDetail() {
  const { id } = Route.useParams()
  const { isAdmin } = useAdmin()
  const { data } = useSuspenseQuery(ec2InstanceQueryOptions(id))
  const instance = data.Reservations?.[0]?.Instances?.[0]

  const { data: imageData } = useSuspenseQuery(
    ec2ImageQueryOptions(instance?.ImageId),
  )
  const image = imageData?.Images?.[0]

  const { data: instanceTypesData } = useSuspenseQuery(
    ec2InstanceTypesQueryOptions,
  )

  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    enabled: isAdmin,
    refetchInterval: 5000,
  })
  const vmInfo = vmsData?.vms?.find((v) => v.instance_id === id)

  const modifyMutation = useModifyInstanceAttribute()
  const consoleMutation = useGetConsoleOutput()

  const [showChangeType, setShowChangeType] = useState(false)
  const [selectedType, setSelectedType] = useState("")
  const [showConsole, setShowConsole] = useState(false)
  const [consoleOutput, setConsoleOutput] = useState<string | null>(null)
  const [showCreateImage, setShowCreateImage] = useState(false)

  const instanceTypes = [
    ...new Set(
      instanceTypesData.InstanceTypes?.flatMap((t) =>
        t.InstanceType ? [t.InstanceType] : [],
      ),
    ),
  ].toSorted()

  if (!instance?.InstanceId) {
    return (
      <>
        <BackLink to="/ec2/describe-instances">Back to instances</BackLink>
        <p className="text-muted-foreground">Instance not found.</p>
      </>
    )
  }

  const launchTime = formatDateTime(instance.LaunchTime)
  const isRunning = instance.State?.Name === "running"
  const isStopped = instance.State?.Name === "stopped"

  async function handleChangeType() {
    try {
      await modifyMutation.mutateAsync({
        instanceId: id,
        instanceType: selectedType,
      })
      setShowChangeType(false)
      setSelectedType("")
    } catch {
      // error shown via modifyMutation.error
    }
  }

  async function handleGetConsoleOutput() {
    try {
      const result = await consoleMutation.mutateAsync(id)
      const decoded = result.Output ? atob(result.Output) : "(no output)"
      setConsoleOutput(decoded)
      setShowConsole(true)
    } catch {
      // error shown via consoleMutation.error
    }
  }

  return (
    <>
      <BackLink to="/ec2/describe-instances">Back to instances</BackLink>

      {modifyMutation.error && (
        <ErrorBanner
          error={modifyMutation.error}
          msg="Failed to modify instance"
        />
      )}
      {consoleMutation.error && (
        <ErrorBanner
          error={consoleMutation.error}
          msg="Failed to get console output"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              {isRunning && (
                <Button
                  disabled={consoleMutation.isPending}
                  onClick={handleGetConsoleOutput}
                  size="sm"
                  variant="outline"
                >
                  <Terminal className="size-4" />
                  {consoleMutation.isPending
                    ? "Fetching\u2026"
                    : "View Console"}
                </Button>
              )}
              {(isRunning || isStopped) && (
                <Button
                  onClick={() => setShowCreateImage(true)}
                  size="sm"
                  variant="outline"
                >
                  <ImagePlus className="size-4" />
                  Create Image
                </Button>
              )}
              {isStopped && (
                <Button
                  onClick={() => {
                    setSelectedType(instance.InstanceType ?? "")
                    setShowChangeType(true)
                  }}
                  size="sm"
                  variant="outline"
                >
                  <Settings2 className="size-4" />
                  Modify Instance
                </Button>
              )}
              <StateBadge state={instance.State?.Name} />
            </div>
          }
          subtitle="EC2 Instance Details"
          title={instance.InstanceId}
        />

        <InstanceActions
          instanceId={instance.InstanceId}
          state={instance.State?.Name}
        />

        {/* Console Output */}
        {showConsole && consoleOutput !== null && (
          <DetailCard>
            <DetailCard.Header>
              <div className="flex items-center justify-between">
                Console Output
                <Button
                  onClick={() => {
                    setShowConsole(false)
                    setConsoleOutput(null)
                  }}
                  size="sm"
                  variant="ghost"
                >
                  Hide
                </Button>
              </div>
            </DetailCard.Header>
            <div className="p-4">
              <pre className="max-h-96 overflow-y-auto rounded bg-muted p-3 font-mono text-xs whitespace-pre-wrap">
                {consoleOutput}
              </pre>
            </div>
          </DetailCard>
        )}

        {/* Instance Details */}
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
            <DetailRow label="Launch Time" value={launchTime} />
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

        {/* Network & Security */}
        {/* oxlint-disable typescript/prefer-nullish-coalescing */}
        {(instance.PrivateIpAddress ||
          instance.PublicIpAddress ||
          instance.VpcId ||
          instance.SubnetId ||
          (instance.SecurityGroups && instance.SecurityGroups.length > 0)) && (
          /* oxlint-enable typescript/prefer-nullish-coalescing */
          <DetailCard>
            <DetailCard.Header>Network & Security</DetailCard.Header>
            <DetailCard.Content>
              <DetailRow
                label="Private IP Address"
                value={instance.PrivateIpAddress}
              />
              <DetailRow
                label="Public IP Address"
                value={instance.PublicIpAddress}
              />
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
                value={instance.SecurityGroups?.map((sg) => sg.GroupName).join(
                  ", ",
                )}
              />
            </DetailCard.Content>
          </DetailCard>
        )}

        {/* GPU */}
        {isAdmin && vmInfo?.gpu && (
          <DetailCard>
            <DetailCard.Header>GPU</DetailCard.Header>
            <DetailCard.Content>
              <DetailRow label="Model" value={vmInfo.gpu.model} />
              <DetailRow
                label="VRAM"
                value={formatVRAMMiB(vmInfo.gpu.vram_mib)}
              />
              <DetailRow
                label="Attachment"
                value={vmInfo.gpu.profile ? "MIG slice" : "PCIe passthrough"}
              />
              {vmInfo.gpu.profile && (
                <DetailRow label="Profile" value={vmInfo.gpu.profile} />
              )}
              {vmInfo.gpu.mdev_path && (
                <DetailRow label="Mdev path" value={vmInfo.gpu.mdev_path} />
              )}
              {vmInfo.gpu.pci_address && (
                <DetailRow label="PCI address" value={vmInfo.gpu.pci_address} />
              )}
            </DetailCard.Content>
          </DetailCard>
        )}

        {/* Block Device Mappings */}
        {instance.BlockDeviceMappings &&
          instance.BlockDeviceMappings.length > 0 && (
            <DetailCard>
              <DetailCard.Header>Block Device Mappings</DetailCard.Header>
              {instance.BlockDeviceMappings.map((bdm) => (
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
          )}

        {/* AMI Details */}
        {image && <AmiDetails image={image} />}
      </div>

      <AlertDialog
        onOpenChange={(open) => {
          if (!open) {
            setShowChangeType(false)
            setSelectedType("")
          }
        }}
        open={showChangeType}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Modify Instance</AlertDialogTitle>
            <AlertDialogDescription>
              Select a new instance type for &quot;{instance.InstanceId}&quot;.
              The instance must be stopped to modify its attributes.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <Field>
            <FieldTitle>
              <label htmlFor="instanceType">Instance Type</label>
            </FieldTitle>
            <Select
              onValueChange={(value) => setSelectedType(value ?? "")}
              value={selectedType}
            >
              <SelectTrigger className="w-full" id="instanceType">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {instanceTypes.map((type) => (
                  <SelectItem key={type} value={type}>
                    {type}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={
                modifyMutation.isPending ||
                !selectedType ||
                selectedType === instance.InstanceType
              }
              onClick={handleChangeType}
            >
              {modifyMutation.isPending ? "Saving\u2026" : "Save Changes"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <CreateImageDialog
        instanceId={instance.InstanceId}
        onOpenChange={setShowCreateImage}
        open={showCreateImage}
      />
    </>
  )
}
