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
import { Textarea } from "@/components/ui/textarea"
import { useCreateImage } from "@/mutations/ec2"

interface CreateImageDialogProps {
  instanceId: string
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function CreateImageDialog({
  instanceId,
  open,
  onOpenChange,
}: CreateImageDialogProps) {
  const createImageMutation = useCreateImage()
  const [imageName, setImageName] = useState("")
  const [imageDescription, setImageDescription] = useState("")
  const [createdImageId, setCreatedImageId] = useState<string | null>(null)

  function handleClose(nextOpen: boolean) {
    if (!nextOpen) {
      setImageName("")
      setImageDescription("")
      setCreatedImageId(null)
    }
    onOpenChange(nextOpen)
  }

  async function handleCreate() {
    try {
      const result = await createImageMutation.mutateAsync({
        instanceId,
        name: imageName,
        description: imageDescription || undefined,
      })
      setCreatedImageId(result.ImageId ?? null)
      setImageName("")
      setImageDescription("")
    } catch {
      // error shown via createImageMutation.error
    }
  }

  return (
    <AlertDialog onOpenChange={handleClose} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {createdImageId ? "Image Created" : "Create Image (AMI)"}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {createdImageId
              ? `Image "${createdImageId}" was created successfully.`
              : `Create an AMI from instance "${instanceId}".`}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {!createdImageId && (
          <>
            <Field>
              <FieldTitle>
                <label htmlFor="imageName">Name</label>
              </FieldTitle>
              <Input
                id="imageName"
                onChange={(e) => {
                  setImageName(e.target.value)
                }}
                placeholder="my-image"
                value={imageName}
              />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="imageDescription">Description</label>
              </FieldTitle>
              <Textarea
                id="imageDescription"
                onChange={(e) => {
                  setImageDescription(e.target.value)
                }}
                placeholder="Optional description"
                rows={2}
                value={imageDescription}
              />
            </Field>
          </>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>
            {createdImageId ? "Close" : "Cancel"}
          </AlertDialogCancel>
          {!createdImageId && (
            <AlertDialogAction
              disabled={createImageMutation.isPending || !imageName.trim()}
              onClick={handleCreate}
            >
              {createImageMutation.isPending
                ? "Creating\u2026"
                : "Create Image"}
            </AlertDialogAction>
          )}
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
