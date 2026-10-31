import { zodResolver } from "@hookform/resolvers/zod"
import { useForm, useWatch } from "react-hook-form"
import { z } from "zod"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateCluster } from "@/mutations/ecs"

const createClusterSchema = z.object({
  name: z.string().trim().min(1, "Enter a cluster name"),
})

type CreateClusterFormData = z.infer<typeof createClusterSchema>

interface CreateClusterPageProps {
  // Called with the new cluster's name once created, or nothing on cancel.
  onClose: (clusterName?: string) => void
}

export function CreateClusterPage({ onClose }: CreateClusterPageProps) {
  const createCluster = useCreateCluster()

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreateClusterFormData>({
    resolver: zodResolver(createClusterSchema),
    defaultValues: { name: "" },
  })

  const name = useWatch({ control, name: "name" })

  const onSubmit = async (data: CreateClusterFormData) => {
    try {
      const result = await createCluster.mutateAsync(data.name)

      onClose(result.cluster?.clusterName ?? data.name)
    } catch {
      // Left open, with the refusal shown beside the submit button.
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createCluster.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="clusterName">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.name}
            id="clusterName"
            placeholder="my-cluster"
            {...register("name")}
          />
          <FieldDescription>
            A cluster groups the services, tasks and container instances that
            run on it. Capacity is provisioned once the cluster exists.
          </FieldDescription>
          <FieldError errors={[errors.name]} />
        </Field>

        <CliCommandPanel commands={buildCreateClusterCommands(name)} />

        <FormActions
          error={createCluster.error}
          errorMsg="Failed to create cluster"
          isPending={createCluster.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create cluster"
        />
      </FormLock>
    </form>
  )
}

function buildCreateClusterCommands(name: string): CliCommand[] {
  return [
    {
      label: "Create cluster",
      parts: [
        { type: "bin", value: "AWS_PROFILE=spinifex aws ecs create-cluster" },
        ...commandFlag(
          "\\\n  --cluster-name",
          cliPlaceholder(name.trim(), "name"),
        ),
      ],
    },
  ]
}
