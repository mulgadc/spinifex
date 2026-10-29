import type { Instance } from "@aws-sdk/client-ec2"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import {
  ChevronDown,
  ImagePlus,
  Maximize2,
  Pause,
  Play,
  RotateCw,
  Settings2,
  Terminal,
  Trash2,
} from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { GpuInstanceTypeSelect } from "@/components/gpu-instance-type-select"
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
import { ButtonGroup } from "@/components/ui/button-group"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Field, FieldTitle } from "@/components/ui/field"
import type { PanelTab } from "@/lib/panel-search"
import {
  useModifyInstanceAttribute,
  useRebootInstance,
  useStartInstance,
  useStopInstance,
  useTerminateInstance,
} from "@/mutations/ec2"
import {
  ec2ConsoleOutputQueryOptions,
  ec2InstanceTypesQueryOptions,
} from "@/queries/ec2"

import { CreateImageDialog } from "../../-components/create-image-dialog"

type Dialog = "" | "stop" | "reboot" | "terminate" | "type" | "image"

interface InstanceActionsProps {
  instance: Instance
  layout: "auto" | "page"
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  onTerminated: () => void
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

// Start, Stop and Reboot stay in view and disable by state, so the user sees
// what exists; the rarer actions sit behind the Actions menu beside them.
export function InstanceActions({
  instance,
  layout,
  tab,
  onTabChange,
  onTerminated,
}: InstanceActionsProps) {
  const id = instance.InstanceId ?? ""
  const state = instance.State?.Name ?? ""
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const start = useStartInstance()
  const stop = useStopInstance()
  const reboot = useRebootInstance()
  const terminate = useTerminateInstance()
  const [dialog, setDialog] = useState<Dialog>("")
  const error = start.error ?? stop.error ?? reboot.error ?? terminate.error

  function closeDialog(open: boolean) {
    if (!open) {
      setDialog("")
    }
  }

  return (
    <>
      <div className="flex flex-wrap items-center justify-center gap-2">
        <ButtonGroup aria-label="Power">
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
              setDialog("stop")
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
              setDialog("reboot")
            }}
            size="sm"
            variant="outline"
          >
            <RotateCw data-icon="inline-start" />
            Reboot
          </Button>
        </ButtonGroup>
        <DropdownMenu>
          <DropdownMenuTrigger render={<Button size="sm" variant="outline" />}>
            Actions
            <ChevronDown data-icon="inline-end" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-48">
            {layout === "auto" && (
              <DropdownMenuItem
                onClick={() => {
                  void navigate({
                    to: "/ec2/describe-instances/$id",
                    params: { id },
                    search: { tab },
                  })
                }}
              >
                <Maximize2 />
                Open instance page
              </DropdownMenuItem>
            )}
            <DropdownMenuItem
              disabled={state !== "running"}
              onClick={() => {
                onTabChange("settings")
                void queryClient.query({
                  ...ec2ConsoleOutputQueryOptions(id),
                  staleTime: 0,
                })
              }}
            >
              <Terminal />
              View console output
            </DropdownMenuItem>
            <DropdownMenuItem
              disabled={state !== "stopped"}
              onClick={() => {
                setDialog("type")
              }}
            >
              <Settings2 />
              Change instance type
            </DropdownMenuItem>
            <DropdownMenuItem
              disabled={state !== "running" && state !== "stopped"}
              onClick={() => {
                setDialog("image")
              }}
            >
              <ImagePlus />
              Create image
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuItem
              disabled={
                ["terminated", "shutting-down"].includes(state) ||
                terminate.isPending
              }
              onClick={() => {
                setDialog("terminate")
              }}
              variant="destructive"
            >
              <Trash2 />
              Terminate
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      {error && <ErrorBanner error={error} />}

      <DeleteConfirmationDialog
        confirmLabel="Stop"
        description={`Stopping ${id} shuts its guest down. Its volumes are kept, and Start brings it back.`}
        isPending={stop.isPending}
        onConfirm={() => {
          stop.mutate(id, {
            onSettled: () => {
              setDialog("")
            },
          })
        }}
        onOpenChange={closeDialog}
        open={dialog === "stop"}
        pendingLabel="Stopping…"
        title="Stop instance?"
      />
      <DeleteConfirmationDialog
        confirmLabel="Reboot"
        description={`Rebooting ${id} restarts its guest. Anything not written to disk is lost.`}
        isPending={reboot.isPending}
        onConfirm={() => {
          reboot.mutate(id, {
            onSettled: () => {
              setDialog("")
            },
          })
        }}
        onOpenChange={closeDialog}
        open={dialog === "reboot"}
        pendingLabel="Rebooting…"
        title="Reboot instance?"
      />
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
