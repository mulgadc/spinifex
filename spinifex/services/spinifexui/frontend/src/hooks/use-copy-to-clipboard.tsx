import { useEffect, useRef, useState } from "react"

const COPY_FEEDBACK_DURATION_MS = 2000

export function useCopyToClipboard() {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout>>(null)

  useEffect(
    () => () => {
      if (timerRef.current) {
        clearTimeout(timerRef.current)
      }
    },
    [],
  )

  const copy = async (text: string) => {
    await navigator.clipboard.writeText(text)
    setCopied(true)
    if (timerRef.current) {
      clearTimeout(timerRef.current)
    }
    timerRef.current = setTimeout(() => {
      setCopied(false)
    }, COPY_FEEDBACK_DURATION_MS)
  }

  return { copied, copy }
}
