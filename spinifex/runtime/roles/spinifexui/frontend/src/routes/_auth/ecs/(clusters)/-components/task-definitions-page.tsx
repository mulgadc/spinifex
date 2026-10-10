import { PageHeading } from "@/components/page-heading"

import { EcsSectionNav } from "./ecs-section-nav"
import { TaskDefinitionsList } from "./task-definitions-list"

export function TaskDefinitionsPage() {
  return (
    <>
      <PageHeading title="Task definitions" />
      <EcsSectionNav />
      <TaskDefinitionsList />
    </>
  )
}
