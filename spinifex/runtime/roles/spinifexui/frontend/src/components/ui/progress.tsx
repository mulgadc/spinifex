import { Progress as ProgressPrimitive } from "@base-ui/react/progress"

import { cn } from "@/lib/utils"

export function Progress({
  className,
  ...props
}: ProgressPrimitive.Root.Props) {
  return (
    <ProgressPrimitive.Root data-slot="progress" {...props}>
      <ProgressPrimitive.Track
        className={cn(
          "relative h-1.5 w-full overflow-hidden rounded-full bg-muted",
          className,
        )}
        data-slot="progress-track"
      >
        <ProgressPrimitive.Indicator
          className="h-full bg-primary transition-all duration-200 ease-out data-complete:bg-success motion-reduce:transition-none"
          data-slot="progress-indicator"
        />
      </ProgressPrimitive.Track>
    </ProgressPrimitive.Root>
  )
}
