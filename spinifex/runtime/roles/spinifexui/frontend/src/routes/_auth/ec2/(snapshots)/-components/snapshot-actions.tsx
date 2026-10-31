import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { useState } from "react"

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
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { getRegion } from "@/lib/cluster-config"
import { useCopySnapshot, useRegisterImage } from "@/mutations/ec2"
import { ec2RegionsQueryOptions } from "@/queries/ec2"

interface SnapshotDialogProps {
  snapshotId: string
  open: boolean
  onOpenChange: (open: boolean) => void
}

// Copies within the current region, the only one a cluster serves.
export function CopySnapshotDialog({
  snapshotId,
  open,
  onOpenChange,
}: SnapshotDialogProps) {
  const { data } = useQuery(ec2RegionsQueryOptions)
  const copy = useCopySnapshot()
  const [description, setDescription] = useState("")
  const region = data?.Regions?.[0]?.RegionName ?? getRegion()

  function onChange(next: boolean) {
    if (!next) {
      setDescription("")
      copy.reset()
    }

    onOpenChange(next)
  }

  async function onCopy() {
    try {
      await copy.mutateAsync({
        sourceSnapshotId: snapshotId,
        sourceRegion: region,
        description: description || undefined,
      })
      onChange(false)
    } catch {
      // Shown in the dialog.
    }
  }

  return (
    <AlertDialog onOpenChange={onChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Copy snapshot</AlertDialogTitle>
          <AlertDialogDescription>
            Create a copy of {snapshotId} in {region}.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {copy.error && (
          <ErrorBanner error={copy.error} msg="Failed to copy snapshot" />
        )}
        <Field>
          <FieldTitle>
            <label htmlFor="copyDescription">Description (optional)</label>
          </FieldTitle>
          <Input
            id="copyDescription"
            onChange={(e) => {
              setDescription(e.target.value)
            }}
            placeholder="Description for the snapshot copy"
            value={description}
          />
        </Field>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={copy.isPending}
            onClick={() => {
              void onCopy()
            }}
          >
            {copy.isPending ? "Copying…" : "Copy snapshot"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// Registers an AMI and then links to it, rather than closing on success.
export function RegisterImageDialog({
  snapshotId,
  open,
  onOpenChange,
}: SnapshotDialogProps) {
  const register = useRegisterImage()
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")
  const [imageId, setImageId] = useState<string | null>(null)

  function onChange(next: boolean) {
    if (!next) {
      setName("")
      setDescription("")
      setImageId(null)
      register.reset()
    }

    onOpenChange(next)
  }

  async function onRegister() {
    try {
      const result = await register.mutateAsync({
        snapshotId,
        name,
        description: description || undefined,
      })

      setImageId(result.ImageId ?? null)
      setName("")
      setDescription("")
    } catch {
      // Shown in the dialog.
    }
  }

  return (
    <AlertDialog onOpenChange={onChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {imageId ? "Image registered" : "Register as AMI"}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {imageId ? (
              <>
                AMI{" "}
                <Link
                  className="text-primary hover:underline"
                  params={{ id: imageId }}
                  to="/ec2/describe-images/$id"
                >
                  {imageId}
                </Link>{" "}
                was registered successfully.
              </>
            ) : (
              `Create an AMI from snapshot ${snapshotId}.`
            )}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {register.error && (
          <ErrorBanner error={register.error} msg="Failed to register image" />
        )}
        {!imageId && (
          <>
            <Field>
              <FieldTitle>
                <label htmlFor="registerName">Name</label>
              </FieldTitle>
              <Input
                id="registerName"
                onChange={(e) => {
                  setName(e.target.value)
                }}
                placeholder="my-image"
                value={name}
              />
            </Field>
            <Field>
              <FieldTitle>
                <label htmlFor="registerDescription">
                  Description (optional)
                </label>
              </FieldTitle>
              <Textarea
                id="registerDescription"
                onChange={(e) => {
                  setDescription(e.target.value)
                }}
                placeholder="Optional description"
                rows={2}
                value={description}
              />
            </Field>
          </>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel>{imageId ? "Close" : "Cancel"}</AlertDialogCancel>
          {!imageId && (
            <AlertDialogAction
              disabled={register.isPending || !name.trim()}
              onClick={() => {
                void onRegister()
              }}
            >
              {register.isPending ? "Registering…" : "Register AMI"}
            </AlertDialogAction>
          )}
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
