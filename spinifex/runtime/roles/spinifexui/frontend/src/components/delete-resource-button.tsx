import type { LinkProps } from "@tanstack/react-router"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import type { ReactNode } from "react"
import { useState } from "react"

import type { DeleteMutation } from "@/components/resource-delete-dialog"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Button } from "@/components/ui/button"

interface DeleteResourceButtonProps {
  id: string
  mutation: DeleteMutation
  // Where to land once the resource is gone, normally its list page.
  to: LinkProps["to"]
  title: string
  description: ReactNode
  label?: string
  disabled?: boolean
}

export function DeleteResourceButton({
  id,
  mutation,
  to,
  title,
  description,
  label = "Delete",
  disabled = false,
}: DeleteResourceButtonProps) {
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)

  return (
    <>
      <Button
        disabled={disabled || mutation.isPending}
        onClick={() => {
          setOpen(true)
        }}
        size="sm"
        variant="destructive"
      >
        <Trash2 className="size-4" />
        {label}
      </Button>
      <ResourceDeleteDialog
        description={description}
        identifier={id}
        mutation={mutation}
        onDeleted={() => {
          void navigate({ to })
        }}
        onOpenChange={setOpen}
        open={open}
        title={title}
      />
    </>
  )
}
