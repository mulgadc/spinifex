import type {
  GuardrailAssessment,
  GuardrailContentSource,
} from "@aws-sdk/client-bedrock-runtime"
import { useState } from "react"

import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { useApplyGuardrail } from "@/mutations/bedrockRuntime"

interface GuardrailTesterProps {
  guardrailId: string
  guardrailVersion: string
}

const SOURCES: GuardrailContentSource[] = ["INPUT", "OUTPUT"]

function blockedTopics(assessment: GuardrailAssessment): string[] {
  return (assessment.topicPolicy?.topics ?? [])
    .filter((topic) => topic.detected === true || topic.action === "BLOCKED")
    .map((topic) => topic.name ?? "unknown topic")
}

function blockedWords(assessment: GuardrailAssessment): string[] {
  const custom = assessment.wordPolicy?.customWords ?? []
  const managed = assessment.wordPolicy?.managedWordLists ?? []
  return [...custom, ...managed]
    .filter((word) => word.detected === true || word.action === "BLOCKED")
    .map((word) => word.match ?? "unknown word")
}

function AssessmentCard({ assessment }: { assessment: GuardrailAssessment }) {
  const topics = blockedTopics(assessment)
  const words = blockedWords(assessment)

  if (topics.length === 0 && words.length === 0) {
    return null
  }

  return (
    <div className="rounded-md border p-3 text-sm">
      {topics.length > 0 && (
        <p>
          <span className="font-medium">Blocked topics: </span>
          {topics.join(", ")}
        </p>
      )}
      {words.length > 0 && (
        <p>
          <span className="font-medium">Blocked words: </span>
          {words.join(", ")}
        </p>
      )}
    </div>
  )
}

export function GuardrailTester({
  guardrailId,
  guardrailVersion,
}: GuardrailTesterProps) {
  const [text, setText] = useState("")
  const [source, setSource] = useState<GuardrailContentSource>("INPUT")
  const applyGuardrail = useApplyGuardrail()

  async function handleSubmit() {
    if (text.trim().length === 0) {
      return
    }
    try {
      await applyGuardrail.mutateAsync({
        guardrailIdentifier: guardrailId,
        guardrailVersion,
        source,
        text,
      })
    } catch {
      // error shown via applyGuardrail.error
    }
  }

  const outputs = applyGuardrail.data?.outputs ?? []
  const assessments = applyGuardrail.data?.assessments ?? []
  const action = applyGuardrail.data?.action

  return (
    <div className="rounded-lg border bg-card p-4">
      <h2 className="mb-2 font-semibold">Guardrail tester</h2>
      <p className="mb-4 text-sm text-muted-foreground">
        Apply this guardrail (version {guardrailVersion}) to sample content and
        see whether it intervenes.
      </p>

      <form
        className="space-y-2"
        onSubmit={(event) => {
          event.preventDefault()
          void handleSubmit()
        }}
      >
        <fieldset className="flex items-center gap-1 border-0 p-0">
          <legend className="sr-only">Content source</legend>
          {SOURCES.map((candidate) => (
            <Button
              aria-pressed={source === candidate}
              key={candidate}
              onClick={() => {
                setSource(candidate)
              }}
              type="button"
              variant={source === candidate ? "default" : "outline"}
            >
              {candidate}
            </Button>
          ))}
        </fieldset>

        <Label htmlFor="guardrail-content">Content</Label>
        <Textarea
          id="guardrail-content"
          onChange={(event) => {
            setText(event.target.value)
          }}
          placeholder="Enter content to evaluate against this guardrail"
          rows={4}
          value={text}
        />
        <Button
          disabled={applyGuardrail.isPending || text.trim().length === 0}
          type="submit"
        >
          {applyGuardrail.isPending ? "Applying…" : "Apply guardrail"}
        </Button>
      </form>

      {applyGuardrail.error && (
        <p className="mt-4 text-sm text-destructive">
          {applyGuardrail.error.message}
        </p>
      )}

      {applyGuardrail.isSuccess && (
        <div className="mt-4 space-y-3">
          <h3 className="text-sm font-medium">
            Action:{" "}
            <span
              className={
                action === "GUARDRAIL_INTERVENED"
                  ? "text-destructive"
                  : "text-green-600 dark:text-green-400"
              }
            >
              {action}
            </span>
          </h3>

          {assessments.length > 0 && (
            <div className="space-y-2">
              {assessments.map((assessment, index) => (
                // oxlint-disable-next-line no-array-index-key -- assessments have no stable identifier
                <AssessmentCard assessment={assessment} key={index} />
              ))}
            </div>
          )}

          {outputs.length > 0 && (
            <div className="space-y-2">
              <h4 className="text-xs font-medium text-muted-foreground">
                Output
              </h4>
              {outputs.map((output, index) => (
                <pre
                  className="max-h-80 overflow-auto rounded bg-muted p-2 font-mono text-xs break-words whitespace-pre-wrap"
                  // oxlint-disable-next-line no-array-index-key -- outputs have no stable identifier
                  key={index}
                >
                  {output.text ?? "(no text)"}
                </pre>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
