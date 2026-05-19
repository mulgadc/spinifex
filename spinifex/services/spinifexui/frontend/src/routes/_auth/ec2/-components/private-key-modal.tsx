import { useNavigate } from "@tanstack/react-router"
import { AlertTriangle, Check, Download } from "lucide-react"

import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogMedia,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { useCopyToClipboard } from "@/hooks/use-copy-to-clipboard"

interface PrivateKeyModalProps {
  open: boolean
  keyName: string
  keyMaterial: string
}

export function PrivateKeyModal({
  open,
  keyName,
  keyMaterial,
}: PrivateKeyModalProps) {
  const navigate = useNavigate()
  const { copied, copy } = useCopyToClipboard()

  const handleDownload = () => {
    const blob = new Blob([keyMaterial], { type: "text/plain" })
    const url = URL.createObjectURL(blob)
    const link = document.createElement("a")
    link.href = url
    link.download = `${keyName}.pem`
    link.click()
    URL.revokeObjectURL(url)
  }

  const handleClose = () => {
    navigate({ to: "/ec2/describe-key-pairs" })
  }

  return (
    <AlertDialog open={open}>
      <AlertDialogContent className="max-w-2xl">
        <AlertDialogHeader>
          <AlertDialogMedia>
            <AlertTriangle className="text-destructive" />
          </AlertDialogMedia>
          <AlertDialogTitle>Save Your Private Key</AlertDialogTitle>
          <AlertDialogDescription>
            This is your only chance to save the private key file for{" "}
            <strong>{keyName}</strong>. Please download or copy it now. You
            won&apos;t be able to retrieve it again.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <div className="space-y-3">
          <textarea
            aria-label={`Private key material for ${keyName}`}
            className="w-full resize-none overflow-y-auto rounded-md border border-input bg-input/20 px-2 py-2 font-mono text-xs"
            readOnly
            rows={4}
            value={keyMaterial}
          />

          <div className="flex gap-2">
            <Button
              className="flex-1"
              onClick={async () => await copy(keyMaterial)}
              type="button"
              variant="outline"
            >
              {copied ? (
                <>
                  <Check className="mr-2 size-4" />
                  Copied!
                </>
              ) : (
                "Copy to Clipboard"
              )}
            </Button>
            <Button
              className="flex-1"
              onClick={handleDownload}
              type="button"
              variant="outline"
            >
              <Download className="mr-2 size-4" />
              Download .pem File
            </Button>
          </div>
        </div>

        <AlertDialogFooter>
          <AlertDialogCancel onClick={handleClose}>
            Close and Continue
          </AlertDialogCancel>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
