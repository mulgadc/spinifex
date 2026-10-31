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
import { useCreateRepository } from "@/mutations/ecr"

const createRepositorySchema = z.object({
  name: z.string().trim().min(1, "Enter a repository name"),
})

type CreateRepositoryFormData = z.infer<typeof createRepositorySchema>

interface CreateRepositoryPageProps {
  // Called with the new repository's name once created, or nothing on cancel.
  onClose: (repositoryName?: string) => void
}

export function CreateRepositoryPage({ onClose }: CreateRepositoryPageProps) {
  const createRepository = useCreateRepository()

  const {
    control,
    handleSubmit,
    register,
    formState: { errors, isSubmitting },
  } = useForm<CreateRepositoryFormData>({
    resolver: zodResolver(createRepositorySchema),
    defaultValues: { name: "" },
  })

  const name = useWatch({ control, name: "name" })

  const onSubmit = async (data: CreateRepositoryFormData) => {
    try {
      const result = await createRepository.mutateAsync(data.name)

      onClose(result.repository?.repositoryName ?? data.name)
    } catch {
      // Left open, with the refusal shown beside the submit button.
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createRepository.isPending || isSubmitting}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="repositoryName">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.name}
            id="repositoryName"
            placeholder="team/app"
            {...register("name")}
          />
          <FieldDescription>
            Repository names may include namespaces, e.g. team/app.
          </FieldDescription>
          <FieldError errors={[errors.name]} />
        </Field>

        <CliCommandPanel commands={buildCreateRepositoryCommands(name)} />

        <FormActions
          error={createRepository.error}
          errorMsg="Failed to create repository"
          isPending={createRepository.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create repository"
        />
      </FormLock>
    </form>
  )
}

function buildCreateRepositoryCommands(name: string): CliCommand[] {
  return [
    {
      label: "Create repository",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ecr create-repository",
        },
        ...commandFlag(
          "\\\n  --repository-name",
          cliPlaceholder(name.trim(), "name"),
        ),
      ],
    },
  ]
}
