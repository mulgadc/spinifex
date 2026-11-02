import { Link } from "@tanstack/react-router"
import { Plus } from "lucide-react"

import { CreatePanel } from "@/components/create-panel"
import { PageHeading } from "@/components/page-heading"
import { ResourceView } from "@/components/resource-view"
import { buttonVariants } from "@/components/ui/button"
import { CLOSED_PANEL, type PanelSearch } from "@/lib/panel-search"

import { EcsSectionNav } from "./ecs-section-nav"
import { RegisterTaskDefinitionPage } from "./register-task-definition-page"
import { TaskDefinitionsList } from "./task-definitions-list"

interface TaskDefinitionsPageProps {
  search: PanelSearch
  onSearchChange: (next: Partial<PanelSearch>) => void
}

export function TaskDefinitionsPage({
  search,
  onSearchChange,
}: TaskDefinitionsPageProps) {
  function closePanel() {
    onSearchChange(CLOSED_PANEL)
  }

  const panel = search.create ? (
    <CreatePanel onClose={closePanel} title="Register task definition">
      <RegisterTaskDefinitionPage onClose={closePanel} />
    </CreatePanel>
  ) : null

  return (
    <ResourceView panel={panel}>
      <PageHeading
        actions={
          <Link
            className={buttonVariants()}
            from="/ecs/task-definitions"
            search={(previous) => ({
              ...previous,
              ...CLOSED_PANEL,
              create: true,
            })}
            to="."
          >
            <Plus data-icon="inline-start" />
            Register task definition
          </Link>
        }
        title="Task Definitions"
      />
      <EcsSectionNav />
      <TaskDefinitionsList />
    </ResourceView>
  )
}
