import { AlertTriangle, Check } from "lucide-react"

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

interface AccessKeyModalProps {
  onClose: () => void
  accessKeyId: string
  secretAccessKey: string
}

export function AccessKeyModal({
  onClose,
  accessKeyId,
  secretAccessKey,
}: AccessKeyModalProps) {
  const { copied, copy } = useCopyToClipboard()

  return (
    <AlertDialog open>
      <AlertDialogContent className="max-w-2xl">
        <AlertDialogHeader>
          <AlertDialogMedia>
            <AlertTriangle className="text-destructive" />
          </AlertDialogMedia>
          <AlertDialogTitle>Save Your Access Key</AlertDialogTitle>
          <AlertDialogDescription>
            This is your only chance to view the secret access key. Please copy
            it now. You won&apos;t be able to retrieve it again.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <div className="space-y-3">
          <div className="space-y-2 rounded-md border p-4 font-mono text-sm">
            <div>
              <span className="text-muted-foreground">Access Key ID: </span>
              {accessKeyId}
            </div>
            <div>
              <span className="text-muted-foreground">Secret Access Key: </span>
              {secretAccessKey}
            </div>
          </div>

          <Button
            className="w-full"
            onClick={async () =>
              await copy(
                `Access Key ID: ${accessKeyId}\nSecret Access Key: ${secretAccessKey}`,
              )
            }
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
        </div>

        <AlertDialogFooter>
          <AlertDialogCancel onClick={onClose}>
            Close and Continue
          </AlertDialogCancel>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
