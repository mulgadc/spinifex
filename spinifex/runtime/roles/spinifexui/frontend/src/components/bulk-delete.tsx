import { CircleCheck, CircleX, LoaderCircle, Trash2 } from "lucide-react"
import { type ReactNode, useState } from "react"

import { ConfirmPhrase, phraseConfirmed } from "@/components/confirm-phrase"
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
import { Button } from "@/components/ui/button"

type Outcome =
  | { status: "running" }
  | { status: "done" }
  | { status: "failed"; message: string }

// Most AWS delete calls take one ID, so the selection is deleted one call at a
// time and each failure kept. A call that takes many, such as DeleteObjects,
// passes deleteMany instead and returns the IDs it could not delete, with why.
type Deleter =
  | { deleteOne: (id: string) => Promise<object>; deleteMany?: never }
  | {
      deleteMany: (
        ids: readonly string[],
      ) => Promise<ReadonlyMap<string, string>>
      deleteOne?: never
    }

type BulkDeleteProps = Deleter & {
  ids: readonly string[]
  // Singular and plural, as "key pair" and "key pairs".
  noun: readonly [string, string]
  // How a row is named in the dialog, when its ID alone says too little.
  label?: (id: string) => string
  description?: ReactNode
  // "Delete", "deleted" and "Deleting"; "Release" for an Elastic IP.
  verb?: string
  pastVerb?: string
  pendingVerb?: string
  // Typed before the confirm enables, for deletes that cannot be walked back.
  confirmPhrase?: string
  // The IDs that were deleted, once a run ends, so the caller can drop them
  // from its selection.
  onDeleted: (ids: string[]) => void
}

function byId(id: string): string {
  return id
}

// The bulk bar's destructive button and its confirmation. A partial failure
// leaves the dialog open with each refusal against its resource, and a second
// confirm retries only those.
export function BulkDelete({
  ids,
  noun,
  label = byId,
  description,
  verb = "Delete",
  pastVerb = "deleted",
  pendingVerb = "Deleting",
  confirmPhrase,
  deleteOne,
  deleteMany,
  onDeleted,
}: BulkDeleteProps) {
  const [open, setOpen] = useState(false)
  // The selection as it stood on opening: deleted rows leave the list as
  // each call lands, but the dialog keeps showing them as done.
  const [batch, setBatch] = useState<readonly string[]>([])

  const [labels, setLabels] = useState<ReadonlyMap<string, string>>(
    () => new Map(),
  )

  const [running, setRunning] = useState(false)
  const [typed, setTyped] = useState("")

  const [outcomes, setOutcomes] = useState<ReadonlyMap<string, Outcome>>(
    () => new Map(),
  )

  const failed = batch.filter((id) => outcomes.get(id)?.status === "failed")
  const targets = failed.length > 0 ? failed : batch
  const name = targets.length === 1 ? noun[0] : noun[1]

  function handleOpenChange(next: boolean) {
    if (running) {
      return
    }

    if (!next) {
      setOutcomes(new Map())
    }

    setOpen(next)
  }

  async function run() {
    setRunning(true)

    const deleted: string[] = []
    const next = new Map(outcomes)

    if (deleteMany) {
      for (const id of targets) {
        next.set(id, { status: "running" })
      }

      setOutcomes(new Map(next))
      let refused: ReadonlyMap<string, string>

      try {
        refused = await deleteMany(targets)
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error)
        refused = new Map(targets.map((id) => [id, message]))
      }

      for (const id of targets) {
        const message = refused.get(id)

        if (message === undefined) {
          next.set(id, { status: "done" })
          deleted.push(id)
        } else {
          next.set(id, { status: "failed", message })
        }
      }

      setOutcomes(new Map(next))
    }

    if (deleteOne) {
      for (const id of targets) {
        next.set(id, { status: "running" })
        setOutcomes(new Map(next))

        try {
          // One at a time, so each refusal lands against its own resource.
          // oxlint-disable-next-line no-await-in-loop
          await deleteOne(id)
          next.set(id, { status: "done" })
          deleted.push(id)
        } catch (error) {
          next.set(id, {
            status: "failed",
            message: error instanceof Error ? error.message : String(error),
          })
        }

        setOutcomes(new Map(next))
      }
    }

    setRunning(false)
    onDeleted(deleted)

    if (deleted.length === targets.length) {
      setOutcomes(new Map())
      setOpen(false)
    }
  }

  let action = `${verb} ${targets.length}`

  if (running) {
    action = `${pendingVerb}…`
  } else if (failed.length > 0) {
    action = `Retry ${failed.length}`
  }

  return (
    <>
      <Button
        disabled={ids.length === 0}
        onClick={() => {
          setBatch([...ids])
          setTyped("")
          setLabels(new Map(ids.map((id) => [id, label(id)])))
          setOpen(true)
        }}
        size="sm"
        variant="destructive"
      >
        <Trash2 data-icon="inline-start" />
        {verb}
      </Button>
      <AlertDialog onOpenChange={handleOpenChange} open={open}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {failed.length > 0
                ? `${failed.length} could not be ${pastVerb}`
                : `${verb} ${targets.length} ${name}?`}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {failed.length > 0
                ? "The rest are done. Retry the ones that failed, or close and look at them one at a time."
                : (description ?? "This cannot be undone.")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <ul
            aria-label={`Selected ${noun[1]}`}
            className="flex max-h-64 flex-col gap-1.5 overflow-y-auto text-xs"
          >
            {batch.map((id) => {
              const outcome = outcomes.get(id)

              return (
                <li
                  className="flex items-start gap-2"
                  data-status={outcome?.status}
                  key={id}
                >
                  <OutcomeIcon outcome={outcome} />
                  <span className="min-w-0">
                    <span className="block font-mono break-all">
                      {labels.get(id) ?? id}
                    </span>
                    {outcome?.status === "failed" && (
                      <span className="block text-destructive">
                        {outcome.message}
                      </span>
                    )}
                  </span>
                </li>
              )
            })}
          </ul>
          {confirmPhrase && failed.length === 0 && (
            <ConfirmPhrase
              onChange={setTyped}
              phrase={confirmPhrase}
              value={typed}
            />
          )}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={running}>
              {failed.length > 0 ? "Close" : "Cancel"}
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={running || !phraseConfirmed(confirmPhrase, typed)}
              onClick={() => {
                void run()
              }}
              variant="destructive"
            >
              {action}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

function OutcomeIcon({ outcome }: { outcome: Outcome | undefined }) {
  const className = "mt-px size-3.5 shrink-0"

  if (outcome?.status === "running") {
    return (
      <LoaderCircle
        aria-label="In progress"
        className={`${className} animate-spin text-muted-foreground motion-reduce:animate-none`}
      />
    )
  }

  if (outcome?.status === "done") {
    return (
      <CircleCheck aria-label="Done" className={`${className} text-success`} />
    )
  }

  if (outcome?.status === "failed") {
    return (
      <CircleX
        aria-label="Failed"
        className={`${className} text-destructive`}
      />
    )
  }

  return <span aria-hidden className={className} />
}
