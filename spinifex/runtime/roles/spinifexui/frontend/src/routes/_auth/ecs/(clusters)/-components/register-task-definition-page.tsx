import { zodResolver } from "@hookform/resolvers/zod"
import { Plus, Trash2 } from "lucide-react"
import {
  Controller,
  type DeepPartial,
  useFieldArray,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { PanelSection } from "@/components/detail-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import {
  buildContainerDefinition,
  useRegisterTaskDefinition,
} from "@/mutations/ecs"
import {
  NETWORK_MODES,
  registerTaskDefinitionSchema,
  type RegisterTaskDefinitionFormData,
} from "@/types/ecs"

interface RegisterTaskDefinitionPageProps {
  // Called with the registered family, or with nothing on cancel.
  onClose: (family?: string) => void
}

export function RegisterTaskDefinitionPage({
  onClose,
}: RegisterTaskDefinitionPageProps) {
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
      containerGpuCount: 0,
      essential: true,
      portMappings: [],
    },
  })

  const ports = useFieldArray({ control, name: "portMappings" })
  const values = useWatch({ control })

  async function onSubmit(data: RegisterTaskDefinitionFormData) {
    try {
      await register.mutateAsync(data)
      onClose(data.family)
    } catch {
      // Left open, with the refusal shown beside the submit button.
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={register.isPending || isSubmitting}
      >
        <p className="text-sm text-muted-foreground">
          A task definition with a single container.
        </p>
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

        <PanelSection title="Container">
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
            <div className="grid grid-cols-3 gap-4">
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
              <Field>
                <FieldTitle>
                  <label htmlFor="containerGpuCount">GPU count</label>
                </FieldTitle>
                <Input
                  id="containerGpuCount"
                  min={0}
                  placeholder="0"
                  type="number"
                  {...field("containerGpuCount", { valueAsNumber: true })}
                />
                <FieldError errors={[errors.containerGpuCount]} />
              </Field>
            </div>
            <div className="flex items-center gap-2">
              <Controller
                control={control}
                name="essential"
                render={({ field: { onChange, value } }) => (
                  <Switch
                    checked={value}
                    id="essential"
                    onCheckedChange={onChange}
                  />
                )}
              />
              <Label htmlFor="essential">Essential</Label>
            </div>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">Port mappings</span>
                <Button
                  onClick={() => {
                    ports.append({ containerPort: 80, protocol: "tcp" })
                  }}
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
                    onClick={() => {
                      ports.remove(i)
                    }}
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
        </PanelSection>

        <CliCommandPanel
          commands={buildRegisterTaskDefinitionCommands(values)}
        />

        <FormActions
          error={register.error}
          errorMsg="Failed to register task definition"
          isPending={register.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Registering…"
          submitLabel="Register task definition"
        />
      </FormLock>
    </form>
  )
}

function buildRegisterTaskDefinitionCommands(
  values: DeepPartial<RegisterTaskDefinitionFormData>,
): CliCommand[] {
  const container = buildContainerDefinition({
    family: "",
    networkMode: "bridge",
    cpu: "",
    memory: "",
    containerName: values.containerName ?? "",
    image: values.image ?? "",
    containerCpu: values.containerCpu ?? "",
    containerMemory: values.containerMemory ?? "",
    containerGpuCount: Number.isFinite(values.containerGpuCount)
      ? (values.containerGpuCount ?? 0)
      : 0,
    essential: values.essential ?? true,
    portMappings: (values.portMappings ?? []).flatMap((pm) =>
      pm?.containerPort && pm.protocol
        ? [{ containerPort: pm.containerPort, protocol: pm.protocol }]
        : [],
    ),
  })

  return [
    {
      label: "Register task definition",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ecs register-task-definition",
        },
        ...commandFlag(
          "\\\n  --family",
          cliPlaceholder(values.family ?? "", "family"),
        ),
        ...commandFlag("\\\n  --network-mode", values.networkMode ?? "bridge"),
        ...optionalFlag("\\\n  --cpu", values.cpu),
        ...optionalFlag("\\\n  --memory", values.memory),
        ...commandFlag(
          "\\\n  --container-definitions",
          shellSingleQuote(JSON.stringify([container])),
        ),
      ],
    },
  ]
}
