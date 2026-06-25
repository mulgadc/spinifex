import { zodResolver } from "@hookform/resolvers/zod"
import { useNavigate } from "@tanstack/react-router"
import { Plus, Trash2 } from "lucide-react"
import { Controller, useFieldArray, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useRegisterTaskDefinition } from "@/mutations/ecs"
import {
  NETWORK_MODES,
  registerTaskDefinitionSchema,
  type RegisterTaskDefinitionFormData,
} from "@/types/ecs"

export function RegisterTaskDefinitionPage({ cluster }: { cluster: string }) {
  const navigate = useNavigate()
  const register = useRegisterTaskDefinition()

  const {
    control,
    handleSubmit,
    register: field,
    formState: { errors, isSubmitting },
  } = useForm<RegisterTaskDefinitionFormData>({
    resolver: zodResolver(registerTaskDefinitionSchema),
    defaultValues: {
      family: "",
      networkMode: "bridge",
      cpu: "",
      memory: "",
      containerName: "",
      image: "",
      containerCpu: "",
      containerMemory: "",
      essential: true,
      portMappings: [],
    },
  })
  const ports = useFieldArray({ control, name: "portMappings" })

  function done() {
    void navigate(
      cluster
        ? {
            to: "/ecs/list-clusters/$clusterName",
            params: { clusterName: cluster },
          }
        : { to: "/ecs/list-clusters" },
    )
  }

  async function onSubmit(data: RegisterTaskDefinitionFormData) {
    await register.mutateAsync(data)
    done()
  }

  return (
    <>
      <BackLink to="/ecs/list-clusters">Clusters</BackLink>
      <PageHeading
        subtitle="Register a single-container task definition"
        title="Register Task Definition"
      />

      {register.isError && (
        <ErrorBanner
          error={register.error instanceof Error ? register.error : undefined}
          msg="Failed to register task definition."
        />
      )}

      <form className="max-w-2xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="family">Family</label>
          </FieldTitle>
          <Input id="family" placeholder="my-app" {...field("family")} />
          <FieldError errors={[errors.family]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="networkMode">Network mode</label>
          </FieldTitle>
          <Controller
            control={control}
            name="networkMode"
            render={({ field: f }) => (
              <Select onValueChange={f.onChange} value={f.value}>
                <SelectTrigger className="w-full" id="networkMode">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {NETWORK_MODES.map((mode) => (
                    <SelectItem key={mode} value={mode}>
                      {mode}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
        </Field>

        <div className="grid grid-cols-2 gap-4">
          <Field>
            <FieldTitle>
              <label htmlFor="cpu">Task CPU (units)</label>
            </FieldTitle>
            <Input id="cpu" placeholder="optional" {...field("cpu")} />
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor="memory">Task memory (MiB)</label>
            </FieldTitle>
            <Input id="memory" placeholder="optional" {...field("memory")} />
          </Field>
        </div>

        <div className="rounded-lg border bg-card p-4">
          <h2 className="mb-4 font-semibold">Container</h2>
          <div className="space-y-4">
            <Field>
              <FieldTitle>
                <label htmlFor="containerName">Name</label>
              </FieldTitle>
              <Input
                id="containerName"
                placeholder="app"
                {...field("containerName")}
              />
              <FieldError errors={[errors.containerName]} />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="image">Image</label>
              </FieldTitle>
              <Input
                id="image"
                placeholder="public.ecr.aws/nginx/nginx:latest"
                {...field("image")}
              />
              <FieldError errors={[errors.image]} />
            </Field>
            <div className="grid grid-cols-2 gap-4">
              <Field>
                <FieldTitle>
                  <label htmlFor="containerCpu">CPU (units)</label>
                </FieldTitle>
                <Input
                  id="containerCpu"
                  placeholder="optional"
                  {...field("containerCpu")}
                />
              </Field>
              <Field>
                <FieldTitle>
                  <label htmlFor="containerMemory">Memory (MiB)</label>
                </FieldTitle>
                <Input
                  id="containerMemory"
                  placeholder="optional"
                  {...field("containerMemory")}
                />
              </Field>
            </div>
            <label className="flex items-center gap-2 text-sm">
              <input type="checkbox" {...field("essential")} />
              <span>Essential</span>
            </label>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">Port mappings</span>
                <Button
                  onClick={() =>
                    ports.append({ containerPort: 80, protocol: "tcp" })
                  }
                  size="sm"
                  type="button"
                  variant="outline"
                >
                  <Plus className="size-4" />
                  Add port
                </Button>
              </div>
              {ports.fields.map((row, i) => (
                <div className="flex items-end gap-2" key={row.id}>
                  <Field className="flex-1">
                    <FieldTitle>
                      <label htmlFor={`port-${i}`}>Container port</label>
                    </FieldTitle>
                    <Input
                      id={`port-${i}`}
                      type="number"
                      {...field(`portMappings.${i}.containerPort`, {
                        valueAsNumber: true,
                      })}
                    />
                  </Field>
                  <Controller
                    control={control}
                    name={`portMappings.${i}.protocol`}
                    render={({ field: f }) => (
                      <Select onValueChange={f.onChange} value={f.value}>
                        <SelectTrigger className="w-28">
                          <SelectValue />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="tcp">tcp</SelectItem>
                          <SelectItem value="udp">udp</SelectItem>
                        </SelectContent>
                      </Select>
                    )}
                  />
                  <Button
                    aria-label="Remove port mapping"
                    onClick={() => ports.remove(i)}
                    size="icon"
                    type="button"
                    variant="ghost"
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </div>
              ))}
            </div>
          </div>
        </div>

        <FormActions
          isPending={register.isPending}
          isSubmitting={isSubmitting}
          onCancel={done}
          pendingLabel="Registering…"
          submitLabel="Register Task Definition"
        />
      </form>
    </>
  )
}
