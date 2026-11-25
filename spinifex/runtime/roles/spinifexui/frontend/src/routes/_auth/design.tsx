import { createFileRoute, notFound } from "@tanstack/react-router"

import { pageTitle } from "@/lib/branding"

import { DesignCataloguePage } from "./-components/design/design-catalogue-page"

// The catalogue is for building the console, not using it: a release build
// answers not found, and the dead branch drops the catalogue from the bundle.
export const Route = createFileRoute("/_auth/design")({
  beforeLoad: () => {
    if (!import.meta.env.DEV) {
      notFound({ throw: true })
    }
  },
  head: () => ({
    meta: [{ title: pageTitle("Design system") }],
  }),
  component: import.meta.env.DEV ? DesignCataloguePage : () => null,
})
