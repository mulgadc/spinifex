import { useEffect, useRef } from "react"

import { useSidebar } from "@/components/ui/sidebar"

/**
 * Hook to set the sidebar state only on initial mount
 * @param isOpen - Whether the sidebar should be open or closed
 */
export function useInitialSidebar(isOpen: boolean) {
  const { setOpen } = useSidebar()
  const hasSetSidebar = useRef(false)

  useEffect(() => {
    if (!hasSetSidebar.current) {
      setOpen(isOpen)
      hasSetSidebar.current = true
    }
  }, [setOpen, isOpen])
}
