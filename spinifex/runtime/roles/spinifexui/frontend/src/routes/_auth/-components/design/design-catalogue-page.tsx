import { PageHeading } from "@/components/page-heading"

import { ControlsSection } from "./controls-section"
import { DataSection } from "./data-section"
import { OverlaysSection } from "./overlays-section"
import { TokensSection } from "./tokens-section"

export function DesignCataloguePage() {
  return (
    <>
      <PageHeading
        subtitle="Every token and primitive the console is built from. Switch theme from the header to check both."
        title="Design system"
      />
      <div className="flex flex-col gap-10">
        <TokensSection />
        <ControlsSection />
        <DataSection />
        <OverlaysSection />
      </div>
    </>
  )
}
