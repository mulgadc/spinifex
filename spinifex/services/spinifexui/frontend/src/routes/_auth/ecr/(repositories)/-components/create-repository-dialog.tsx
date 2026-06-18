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
import { useCreateRepository } from "@/mutations/ecr"

interface CreateRepositoryDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function CreateRepositoryDialog({
  open,
  onOpenChange,
}: CreateRepositoryDialogProps) {
  const createRepository = useCreateRepository()
  const [name, setName] = useState("")

  function handleClose(nextOpen: boolean) {
    if (!nextOpen) {
      setName("")
      createRepository.reset()
    }
    onOpenChange(nextOpen)
  }

  async function handleCreate() {
    try {
      await createRepository.mutateAsync(name.trim())
      handleClose(false)
    } catch {
      // error shown via createRepository.error
    }
  }

  return (
    <AlertDialog onOpenChange={handleClose} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Create Repository</AlertDialogTitle>
          <AlertDialogDescription>
            Repository names may include namespaces, e.g. {"team/app"}.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <Field>
          <FieldTitle>
            <label htmlFor="repositoryName">Name</label>
          </FieldTitle>
          <Input
            id="repositoryName"
            onChange={(e) => setName(e.target.value)}
            placeholder="team/app"
            value={name}
          />
        </Field>
        {createRepository.error && (
          <p className="text-sm text-destructive">
            {createRepository.error.message}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={createRepository.isPending || !name.trim()}
            onClick={(e) => {
              e.preventDefault()
              void handleCreate()
            }}
          >
            {createRepository.isPending ? "Creating…" : "Create Repository"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
