import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { useAdmin } from "@/hooks/use-admin"
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

import {
  BlockDeviceMappingsCard,
  ConsoleOutputCard,
  InstanceHeaderActions,
  InstanceInfoCard,
  ModifyInstanceTypeDialog,
  NetworkSecurityCard,
} from "../-components/instance-detail-cards"
import { AmiDetails } from "../../-components/ami-details"
import { CreateImageDialog } from "../../-components/create-image-dialog"
import { GpuDetailCard } from "../../-components/gpu-detail-card"
import { IamRolePanel } from "../../-components/iam-role-panel"
import { InstanceActions } from "../../-components/instance-actions"
import { SSHAccessCard } from "../../-components/ssh-access-card"

export const Route = createFileRoute(
  "/_auth/ec2/(instances)/describe-instances/$id",
)({
  loader: async ({ context, params }) => {
    const [instanceData] = await Promise.all([
      context.queryClient.query({
        ...ec2InstanceQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2InstanceTypesQueryOptions,
        staleTime: "static",
      }),
    ])

    const imageId = instanceData.Reservations?.[0]?.Instances?.[0]?.ImageId
    await context.queryClient.query({
      ...ec2ImageQueryOptions(imageId),
      staleTime: "static",
    })

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

  const { instance, image, instanceTypes, vmInfo } = useInstanceDetailData(
    id,
    isAdmin,
  )

  const modifyMutation = useModifyInstanceAttribute()
  const consoleMutation = useGetConsoleOutput()

  const [showChangeType, setShowChangeType] = useState(false)
  const [selectedType, setSelectedType] = useState("")
  const [showConsole, setShowConsole] = useState(false)
  const [consoleOutput, setConsoleOutput] = useState<string | null>(null)
  const [showCreateImage, setShowCreateImage] = useState(false)

  if (!instance?.InstanceId) {
    return (
      <>
        <BackLink to="/ec2/describe-instances">Back to instances</BackLink>
        <p className="text-muted-foreground">Instance not found.</p>
      </>
    )
  }

  const state = instance.State?.Name

  function closeChangeType() {
    setShowChangeType(false)
    setSelectedType("")
  }

  async function handleChangeType() {
    try {
      await modifyMutation.mutateAsync({
        instanceId: id,
        instanceType: selectedType,
      })
      closeChangeType()
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
            <InstanceHeaderActions
              consolePending={consoleMutation.isPending}
              onCreateImage={() => {
                setShowCreateImage(true)
              }}
              onModify={() => {
                setSelectedType(instance.InstanceType ?? "")
                setShowChangeType(true)
              }}
              onViewConsole={handleGetConsoleOutput}
              state={state}
            />
          }
          subtitle="Instance Details"
          title={instance.InstanceId}
        />

        <InstanceActions instanceId={instance.InstanceId} state={state} />

        {/* SSH Access — only a running instance has a listening sshd */}
        {state === "running" && (
          <SSHAccessCard image={image} instance={instance} />
        )}

        {showConsole && consoleOutput !== null && (
          <ConsoleOutputCard
            onHide={() => {
              setShowConsole(false)
              setConsoleOutput(null)
            }}
            output={consoleOutput}
          />
        )}

        <InstanceInfoCard instance={instance} />
        <NetworkSecurityCard instance={instance} />

        {/* IAM Role */}
        <IamRolePanel instanceId={instance.InstanceId} />

        {/* GPU */}
        {isAdmin && <GpuDetailCard gpus={vmInfo?.gpus} />}

        <BlockDeviceMappingsCard mappings={instance.BlockDeviceMappings} />

        {/* AMI Details */}
        {image && <AmiDetails image={image} />}
      </div>

      <ModifyInstanceTypeDialog
        currentType={instance.InstanceType}
        instanceId={instance.InstanceId}
        instanceTypes={instanceTypes}
        isPending={modifyMutation.isPending}
        onClose={closeChangeType}
        onSave={handleChangeType}
        onSelectedTypeChange={setSelectedType}
        open={showChangeType}
        selectedType={selectedType}
      />

      <CreateImageDialog
        instanceId={instance.InstanceId}
        onOpenChange={setShowCreateImage}
        open={showCreateImage}
      />
    </>
  )
}

const VM_REFRESH_INTERVAL_MS = 5000

function useInstanceDetailData(id: string, isAdmin: boolean) {
  const { data } = useSuspenseQuery(ec2InstanceQueryOptions(id))
  const instance = data.Reservations?.[0]?.Instances?.[0]

  const { data: imageData } = useSuspenseQuery(
    ec2ImageQueryOptions(instance?.ImageId),
  )

  const { data: instanceTypesData } = useSuspenseQuery(
    ec2InstanceTypesQueryOptions,
  )

  const { data: vmsData } = useQuery({
    ...adminVMsQueryOptions,
    enabled: isAdmin,
    refetchInterval: VM_REFRESH_INTERVAL_MS,
  })

  return {
    instance,
    image: imageData?.Images?.[0],
    instanceTypes: instanceTypesData.InstanceTypes ?? [],
    vmInfo: vmsData?.vms?.find((v) => v.instance_id === id),
  }
}
