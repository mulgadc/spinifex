import { ImageUp, Trash2, Undo2 } from "lucide-react"
import { type ReactNode, useId, useRef, useState } from "react"

import { FieldLabel } from "@/components/form-fields"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldError } from "@/components/ui/field"
import type { ImageChange } from "@/types/branding"

interface ImageUploadFieldProps {
  id: string
  label: string
  accept: readonly string[]
  maxBytes: number
  value: ImageChange
  onChange: (change: ImageChange) => void
  // The stored image, if there is one, and where to show it from.
  stored?: { contentType: string; size: number; src?: string }
  description?: ReactNode
  info?: ReactNode
}

function formatBytes(n: number): string {
  return n < 1024 ? `${n} B` : `${(n / 1024).toFixed(1)} KiB`
}

// Base64, as the /admin JSON body carries an image.
async function toBase64(file: File): Promise<string> {
  const bytes = new Uint8Array(await file.arrayBuffer())
  let binary = ""

  for (const byte of bytes) {
    binary += String.fromCodePoint(byte)
  }

  return btoa(binary)
}

// One image file, checked for type and size in the browser before it is sent.
// The gateway checks them again, and the file's own bytes too.
export function ImageUploadField({
  id,
  label,
  accept,
  maxBytes,
  value,
  onChange,
  stored,
  description,
  info,
}: ImageUploadFieldProps) {
  const [error, setError] = useState<string | undefined>()
  const hintId = useId()
  const input = useRef<HTMLInputElement>(null)

  async function choose(file: File | undefined) {
    if (!file) {
      return
    }

    if (!accept.includes(file.type)) {
      setError(
        `Choose a ${accept.map((t) => t.replace("image/", "").replace("+xml", "")).join(", ")} file.`,
      )

      return
    }

    if (file.size > maxBytes) {
      setError(
        `The file is ${formatBytes(file.size)}; at most ${formatBytes(maxBytes)} is allowed.`,
      )

      return
    }

    setError(undefined)
    onChange({
      kind: "set",
      image: {
        contentType: file.type,
        data: await toBase64(file),
        name: file.name,
        size: file.size,
      },
    })
  }

  let status: ReactNode = "Default image"

  if (value.kind === "set") {
    status = `${value.image.name}, ${formatBytes(value.image.size)}, saved on Save`
  } else if (value.kind === "remove") {
    status = "Removed on Save; the default image is used"
  } else if (stored) {
    status = `Stored ${stored.contentType}, ${formatBytes(stored.size)}`
  }

  return (
    <Field>
      <FieldLabel id={id} info={info} label={label} />
      <div className="flex items-center gap-3">
        {value.kind === "keep" && stored?.src && (
          <img
            alt=""
            className="size-8 shrink-0 object-contain"
            src={stored.src}
          />
        )}
        <p
          className="min-w-0 flex-1 truncate text-sm text-muted-foreground"
          id={hintId}
        >
          {status}
        </p>
        <Button
          onClick={() => {
            input.current?.click()
          }}
          type="button"
          variant="outline"
        >
          <ImageUp />
          Choose file
        </Button>
        <input
          accept={accept.join(",")}
          aria-describedby={hintId}
          className="sr-only"
          id={id}
          ref={input}
          onChange={(event) => {
            void choose(event.target.files?.[0])
            event.target.value = ""
          }}
          type="file"
        />
        {value.kind === "keep" ? (
          <Button
            aria-label={`Remove ${label}`}
            disabled={!stored}
            onClick={() => {
              onChange({ kind: "remove" })
            }}
            type="button"
            variant="outline"
          >
            <Trash2 />
          </Button>
        ) : (
          <Button
            aria-label={`Undo ${label} change`}
            onClick={() => {
              setError(undefined)
              onChange({ kind: "keep" })
            }}
            type="button"
            variant="outline"
          >
            <Undo2 />
          </Button>
        )}
      </div>
      {description && <FieldDescription>{description}</FieldDescription>}
      <FieldError errors={error ? [{ message: error }] : []} />
    </Field>
  )
}
