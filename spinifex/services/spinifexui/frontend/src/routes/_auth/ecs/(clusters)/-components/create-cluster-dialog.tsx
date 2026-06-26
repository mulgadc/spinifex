import { useState } from "react"

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
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useCreateCluster } from "@/mutations/ecs"

interface CreateClusterDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function CreateClusterDialog({
  open,
  onOpenChange,
}: CreateClusterDialogProps) {
  const createCluster = useCreateCluster()
  const [name, setName] = useState("")

  function handleClose(nextOpen: boolean) {
    if (!nextOpen) {
      setName("")
      createCluster.reset()
    }
    onOpenChange(nextOpen)
  }

  async function handleCreate() {
    try {
      await createCluster.mutateAsync(name.trim())
      handleClose(false)
    } catch {
      // error shown via createCluster.error
    }
  }

  return (
    <AlertDialog onOpenChange={handleClose} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Create Cluster</AlertDialogTitle>
          <AlertDialogDescription>
            A cluster groups the services, tasks, and container instances that
            run on it.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <Field>
          <FieldTitle>
            <label htmlFor="clusterName">Name</label>
          </FieldTitle>
          <Input
            id="clusterName"
            onChange={(e) => setName(e.target.value)}
            placeholder="my-cluster"
            value={name}
          />
        </Field>
        {createCluster.error && (
          <p className="text-sm text-destructive">
            {createCluster.error.message}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={createCluster.isPending || !name.trim()}
            onClick={(e) => {
              e.preventDefault()
              void handleCreate()
            }}
          >
            {createCluster.isPending ? "Creating…" : "Create Cluster"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
