import { useQueryClient } from "@tanstack/react-query"
import { Upload } from "lucide-react"
import { useRef } from "react"

import { Button } from "@/components/ui/button"
import { percentOf } from "@/lib/transfer"
import { uploads, useUploads } from "@/lib/uploads"

interface UploadButtonProps {
  bucket: string
  // Empty at the bucket root, otherwise ends in "/".
  prefix?: string
}

const RING_RADIUS = 6
const RING_LENGTH = 2 * Math.PI * RING_RADIUS

// The batch's progress as a ring, so the button keeps its width as it fills.
function ProgressRing({ percent }: { percent: number }) {
  return (
    <svg aria-hidden className="size-4 -rotate-90" viewBox="0 0 16 16">
      <circle
        className="stroke-current opacity-25"
        cx="8"
        cy="8"
        fill="none"
        r={RING_RADIUS}
        strokeWidth="2"
      />
      <circle
        className="stroke-current"
        cx="8"
        cy="8"
        fill="none"
        r={RING_RADIUS}
        strokeDasharray={RING_LENGTH}
        strokeDashoffset={RING_LENGTH * (1 - percent / 100)}
        strokeLinecap="round"
        strokeWidth="2"
      />
    </svg>
  )
}

// While a batch runs the button shows its progress, and opens the progress
// dialog; the dialog itself opens on its own for a large or slow batch.
export function UploadButton({ bucket, prefix = "" }: UploadButtonProps) {
  const queryClient = useQueryClient()
  const state = useUploads()
  const fileInputRef = useRef<HTMLInputElement>(null)

  function handleFileSelect(event: React.ChangeEvent<HTMLInputElement>) {
    const files = [...(event.target.files ?? [])]

    uploads.add(bucket, prefix, files, () => {
      void queryClient.invalidateQueries({
        queryKey: ["s3", "buckets", bucket, "objects"],
      })
    })

    // Reset the input so the same file can be uploaded again
    event.target.value = ""
  }

  const percent = percentOf(state.loaded, state.total)

  return (
    <>
      <input
        accept="*/*"
        aria-label="Choose files to upload"
        className="hidden"
        multiple
        onChange={handleFileSelect}
        ref={fileInputRef}
        type="file"
      />
      {state.active ? (
        <Button
          aria-label={`Uploading, ${percent}% done. Show progress`}
          onClick={() => {
            uploads.show(true)
          }}
          variant="outline"
        >
          <ProgressRing percent={percent} />
          <span className="tabular-nums">Uploading {percent}%</span>
        </Button>
      ) : (
        <Button onClick={() => fileInputRef.current?.click()}>
          <Upload data-icon="inline-start" />
          Upload
        </Button>
      )}
    </>
  )
}
