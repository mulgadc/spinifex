import { PageHeading } from "@/components/page-heading"

import { ControlsSection } from "./controls-section"
import { DataSection } from "./data-section"
import { FormsSection } from "./forms-section"
import { OverlaysSection } from "./overlays-section"
import { PatternsSection } from "./patterns-section"
import { RulesSection } from "./rules-section"
import { TokensSection } from "./tokens-section"

export function DesignCataloguePage() {
  return (
    <>
      <PageHeading
        subtitle="Every token, primitive and pattern the console is built from. Switch theme from the header to check both."
        title="Design System"
      />
      <div className="flex flex-col gap-10">
        <RulesSection />
        <TokensSection />
        <ControlsSection />
        <FormsSection />
        <PatternsSection />
        <DataSection />
        <OverlaysSection />
      </div>
    </>
  )
}
