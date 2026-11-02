import type { Instance } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  ChartArea,
  Eye,
  Maximize2,
  RefreshCw,
  SlidersHorizontal,
} from "lucide-react"

import { ConsoleOutput } from "@/components/console-output"
import { DetailPanel, PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { Ec2Tags } from "@/components/resource-tags"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { ec2ConsoleOutputQueryOptions } from "@/queries/ec2"

import { InstanceActions } from "./instance-actions"
import { InstanceMonitoring } from "./instance-monitoring"
import { InstanceOverview } from "./instance-overview"

interface InstanceDetailsProps {
  instance: Instance
  name: string | undefined
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  onClose: () => void
  // The list's side panel, or the instance's own page.
  layout?: "auto" | "page"
}

// The serial log, fetched on request and kept in the query cache, so it is
// still there after the panel expands into the instance's page.
function ConsoleSection({ instance }: { instance: Instance }) {
  const id = instance.InstanceId ?? ""
  const running = instance.State?.Name === "running"

  const { data, error, isFetching, refetch } = useQuery(
    ec2ConsoleOutputQueryOptions(id),
  )

  return (
    <PanelSection title="Console output">
      <div className="flex flex-col items-start gap-3">
        <Button
          disabled={!running || isFetching}
          onClick={() => {
            void refetch()
          }}
          size="sm"
          variant="outline"
        >
          <RefreshCw data-icon="inline-start" />
          {isFetching && "Fetching…"}
          {!isFetching && (data === undefined ? "Fetch output" : "Refresh")}
        </Button>
        {error && <ErrorBanner error={error} />}
        {data !== undefined && (
          <div className="w-full">
            <ConsoleOutput output={data || "(no output)"} />
          </div>
        )}
      </div>
    </PanelSection>
  )
}

// One instance's details, as the list's side panel or as its own page. Both
// render the same tabs, so neither can fall behind the other.
export function InstanceDetails({
  instance,
  name,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: InstanceDetailsProps) {
  const id = instance.InstanceId ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to instances" : "Close instance details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open instance page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-instances/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Instance details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={
        <>
          <StateBadge state={instance.State?.Name} />
          <InstanceActions
            instance={instance}
            layout={layout}
            onTabChange={onTabChange}
            onTerminated={onClose}
            tab={tab}
          />
        </>
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <InstanceOverview instance={instance} />,
        },
        {
          id: "monitoring",
          label: "Monitoring",
          icon: ChartArea,
          content: (
            <InstanceMonitoring
              instanceId={id}
              running={instance.State?.Name === "running"}
            />
          ),
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: (
            <>
              <Ec2Tags
                id={instance.InstanceId}
                invalidate={["ec2", "instances"]}
                tags={instance.Tags}
              />
              <ConsoleSection instance={instance} />
            </>
          ),
        },
      ]}
      title={name ?? id}
    />
  )
}
