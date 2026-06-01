import type { Listener } from "@aws-sdk/client-elastic-load-balancing-v2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Pencil, Trash2 } from "lucide-react"
import { useEffect, useState } from "react"
import { useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ListenerForm } from "@/components/elbv2/listener-form"
import { ErrorBanner } from "@/components/error-banner"
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
import {
  useCreateListener,
  useDeleteListener,
  useModifyListener,
} from "@/mutations/elbv2"
import {
  elbv2ListenersQueryOptions,
  elbv2TargetGroupsQueryOptions,
} from "@/queries/elbv2"
import {
  type CreateListenerFormData,
  createListenerSchema,
} from "@/types/elbv2"

interface ListenersTabProps {
  loadBalancerArn: string
  vpcId: string | undefined
}

export function ListenersTab({ loadBalancerArn, vpcId }: ListenersTabProps) {
  const { data: listenersData } = useSuspenseQuery(
    elbv2ListenersQueryOptions(loadBalancerArn),
  )
  const { data: tgsData } = useSuspenseQuery(elbv2TargetGroupsQueryOptions)

  const createMutation = useCreateListener()
  const deleteMutation = useDeleteListener()
  const modifyMutation = useModifyListener()

  const [addOpen, setAddOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<Listener | undefined>()
  const [deleteTarget, setDeleteTarget] = useState<Listener | undefined>()

  const listeners = listenersData.Listeners ?? []
  const allTgs = tgsData.TargetGroups ?? []
  const vpcTgs = allTgs.filter((tg) => tg.VpcId === vpcId)

  const tgNameByArn = new Map<string, string>()
  for (const tg of allTgs) {
    if (tg.TargetGroupArn && tg.TargetGroupName) {
      tgNameByArn.set(tg.TargetGroupArn, tg.TargetGroupName)
    }
  }

  const formatDefaultAction = (listener: Listener): string => {
    const action = listener.DefaultActions?.[0]
    if (!action) {
      return "—"
    }
    if (action.Type === "forward" && action.TargetGroupArn) {
      const name = tgNameByArn.get(action.TargetGroupArn)
      return name ? `forward → ${name}` : `forward → ${action.TargetGroupArn}`
    }
    return action.Type ?? "—"
  }

  const handleCreate = async (data: CreateListenerFormData) => {
    try {
      await createMutation.mutateAsync({
        loadBalancerArn,
        protocol: data.protocol,
        port: data.port,
        defaultTargetGroupArn: data.defaultTargetGroupArn,
      })
      setAddOpen(false)
    } catch {
      // surfaced via mutation.error
    }
  }

  const handleEdit = async (data: CreateListenerFormData) => {
    if (!editTarget?.ListenerArn) {
      return
    }
    try {
      await modifyMutation.mutateAsync({
        listenerArn: editTarget.ListenerArn,
        loadBalancerArn,
        protocol: data.protocol,
        port: data.port,
        defaultTargetGroupArn: data.defaultTargetGroupArn,
      })
      setEditTarget(undefined)
    } catch {
      // surfaced via mutation.error
    }
  }

  const handleDelete = async () => {
    if (!deleteTarget?.ListenerArn) {
      return
    }
    try {
      await deleteMutation.mutateAsync(deleteTarget.ListenerArn)
    } finally {
      setDeleteTarget(undefined)
    }
  }

  return (
    <div className="space-y-3">
      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create listener"
        />
      )}
      {modifyMutation.error && (
        <ErrorBanner
          error={modifyMutation.error}
          msg="Failed to modify listener"
        />
      )}
      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete listener"
        />
      )}

      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          {listeners.length} listener{listeners.length === 1 ? "" : "s"}
        </p>
        <Button onClick={() => setAddOpen(true)} size="sm">
          Add listener
        </Button>
      </div>

      {listeners.length === 0 ? (
        <p className="text-muted-foreground">No listeners configured.</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Protocol</th>
                <th className="px-4 py-2 font-medium">Port</th>
                <th className="px-4 py-2 font-medium">Default action</th>
                <th className="px-4 py-2 font-medium">Listener ARN</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {listeners.map((listener) => (
                <tr
                  className="border-b last:border-0"
                  key={listener.ListenerArn ?? ""}
                >
                  <td className="px-4 py-2">{listener.Protocol}</td>
                  <td className="px-4 py-2">{listener.Port}</td>
                  <td className="px-4 py-2 text-xs">
                    {formatDefaultAction(listener)}
                  </td>
                  <td className="px-4 py-2 font-mono text-xs">
                    {listener.ListenerArn}
                  </td>
                  <td className="px-4 py-2 text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        aria-label={`Edit listener ${listener.Protocol}:${listener.Port}`}
                        onClick={() => setEditTarget(listener)}
                        size="sm"
                        variant="ghost"
                      >
                        <Pencil className="size-4" />
                      </Button>
                      <Button
                        aria-label={`Delete listener ${listener.Protocol}:${listener.Port}`}
                        onClick={() => setDeleteTarget(listener)}
                        size="sm"
                        variant="ghost"
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <AddListenerDialog
        isPending={createMutation.isPending}
        onOpenChange={setAddOpen}
        onSubmit={handleCreate}
        open={addOpen}
        targetGroups={vpcTgs}
      />

      <EditListenerDialog
        isPending={modifyMutation.isPending}
        listener={editTarget}
        onOpenChange={(open) => !open && setEditTarget(undefined)}
        onSubmit={handleEdit}
        targetGroups={vpcTgs}
      />

      <DeleteConfirmationDialog
        description={
          deleteTarget
            ? `Delete listener ${deleteTarget.Protocol}:${deleteTarget.Port}? This cannot be undone.`
            : ""
        }
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={(open) => !open && setDeleteTarget(undefined)}
        open={deleteTarget !== undefined}
        title="Delete listener"
      />
    </div>
  )
}

interface AddListenerDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  targetGroups: React.ComponentProps<typeof ListenerForm>["targetGroups"]
  isPending: boolean
  onSubmit: (data: CreateListenerFormData) => void
}

function AddListenerDialog({
  open,
  onOpenChange,
  targetGroups,
  isPending,
  onSubmit,
}: AddListenerDialogProps) {
  const form = useForm<CreateListenerFormData>({
    resolver: zodResolver(createListenerSchema),
    defaultValues: {
      protocol: "HTTP",
      port: 80,
      defaultTargetGroupArn: "",
    },
  })

  useEffect(() => {
    if (!open) {
      form.reset({
        protocol: "HTTP",
        port: 80,
        defaultTargetGroupArn: "",
      })
    }
  }, [open, form])

  const handleConfirm = form.handleSubmit(onSubmit)

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent
        className="grid-cols-[minmax(0,1fr)]"
        style={{ maxWidth: "32rem", width: "calc(100vw - 2rem)" }}
      >
        <AlertDialogHeader>
          <AlertDialogTitle>Add listener</AlertDialogTitle>
          <AlertDialogDescription>
            Listeners forward incoming traffic to a default target group.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <form
          className="min-w-0 space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            void handleConfirm()
          }}
        >
          <ListenerForm form={form} targetGroups={targetGroups} />
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel disabled={isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction disabled={isPending} onClick={handleConfirm}>
            {isPending ? "Adding\u2026" : "Add listener"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

interface EditListenerDialogProps {
  listener: Listener | undefined
  onOpenChange: (open: boolean) => void
  targetGroups: React.ComponentProps<typeof ListenerForm>["targetGroups"]
  isPending: boolean
  onSubmit: (data: CreateListenerFormData) => void
}

function EditListenerDialog({
  listener,
  onOpenChange,
  targetGroups,
  isPending,
  onSubmit,
}: EditListenerDialogProps) {
  const open = listener !== undefined

  const form = useForm<CreateListenerFormData>({
    resolver: zodResolver(createListenerSchema),
    defaultValues: {
      protocol: "HTTP",
      port: 80,
      defaultTargetGroupArn: "",
    },
  })

  useEffect(() => {
    if (!listener) {
      return
    }
    form.reset({
      protocol: "HTTP",
      port: listener.Port ?? 80,
      defaultTargetGroupArn: listener.DefaultActions?.[0]?.TargetGroupArn ?? "",
    })
  }, [listener, form])

  const handleConfirm = form.handleSubmit(onSubmit)

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent
        className="grid-cols-[minmax(0,1fr)]"
        style={{ maxWidth: "32rem", width: "calc(100vw - 2rem)" }}
      >
        <AlertDialogHeader>
          <AlertDialogTitle>Edit listener</AlertDialogTitle>
          <AlertDialogDescription>
            Update port, protocol, or default target group. Changes apply
            without dropping the listener.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <form
          className="min-w-0 space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            void handleConfirm()
          }}
        >
          <ListenerForm form={form} targetGroups={targetGroups} />
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel disabled={isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction disabled={isPending} onClick={handleConfirm}>
            {isPending ? "Saving\u2026" : "Save changes"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
