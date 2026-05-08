import { Upload } from "lucide-react"
import { useRef } from "react"

import { Button } from "@/components/ui/button"

interface UploadButtonProps {
  bucket: string
  prefix?: string
  isPending: boolean
  onUpload: (params: {
    bucket: string
    key: string
    file: File
  }) => Promise<unknown>
}

export function UploadButton({
  bucket,
  prefix = "",
  isPending,
  onUpload,
}: UploadButtonProps) {
  const fileInputRef = useRef<HTMLInputElement>(null)

  async function handleFileSelect(event: React.ChangeEvent<HTMLInputElement>) {
    const files = event.target.files
    if (!files || files.length === 0) {
      return
    }

    try {
      await Promise.all(
        [...files].map(async (file) => {
          const key = `${prefix}${file.name}`
          return await onUpload({ bucket, key, file })
        }),
      )
    } finally {
      // Reset the input so the same file can be uploaded again
      if (fileInputRef.current) {
        fileInputRef.current.value = ""
      }
    }
  }

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
      <Button
        disabled={isPending}
        onClick={() => fileInputRef.current?.click()}
        size="sm"
      >
        <Upload className="mr-2 size-4" />
        {isPending ? "Uploading…" : "Upload Files"}
      </Button>
    </>
  )
}
