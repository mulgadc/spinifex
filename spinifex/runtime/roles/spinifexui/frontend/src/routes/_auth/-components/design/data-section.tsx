import { useState } from "react"

import { StateBadge } from "@/components/state-badge"
import { RelativeTime, TagPills, TruncatedText } from "@/components/table-cells"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsList, TabsPanel, TabsTab } from "@/components/ui/tabs"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

import { CatalogueSection, Specimen } from "./catalogue-section"

const TABLE_STATES = ["rows", "many", "long", "loading", "empty", "error"]

type TableState = (typeof TABLE_STATES)[number]

interface SampleInstance {
  id: string
  name: string
  state: string
  type: string
  address: string
}

const STATES = ["running", "pending", "stopped", "running", "terminated"]

function sampleInstances(count: number): SampleInstance[] {
  return Array.from({ length: count }, (_, index) => ({
    id: `i-0${(0xa1_b2_c3_d4 + index).toString(16)}e5f6789`,
    name: `web-${String(index + 1).padStart(2, "0")}`,
    state: STATES[index % STATES.length] ?? "running",
    type: index % 3 === 0 ? "t3.xlarge" : "t3.micro",
    address: `10.200.${Math.floor(index / 250)}.${(index % 250) + 4}`,
  }))
}

const FEW = sampleInstances(5)
const MANY = sampleInstances(60)

const LONG: SampleInstance[] = [
  {
    id: "i-0a1b2c3d4e5f67890",
    name: "a-very-long-instance-name-that-a-customer-chose-for-their-production-database-replica",
    state: "running",
    type: "t3.xlarge",
    address: "10.200.0.4",
  },
]

function rowsFor(state: TableState): SampleInstance[] {
  if (state === "many") {
    return MANY
  }

  if (state === "long") {
    return LONG
  }

  return FEW
}

function InstanceRows({ state }: { state: TableState }) {
  if (state === "loading") {
    return ["a", "b", "c"].map((key) => (
      <TableRow key={key}>
        <TableCell colSpan={5}>
          <Skeleton className="h-4 w-full" />
        </TableCell>
      </TableRow>
    ))
  }

  if (state === "empty") {
    return (
      <TableRow>
        <TableCell colSpan={5}>
          <p className="py-8 text-center text-muted-foreground">
            No instances in this region.
          </p>
        </TableCell>
      </TableRow>
    )
  }

  if (state === "error") {
    return (
      <TableRow>
        <TableCell colSpan={5}>
          <p className="py-8 text-center text-danger-foreground">
            Could not list instances: the gateway did not answer.
          </p>
        </TableCell>
      </TableRow>
    )
  }

  return rowsFor(state).map((instance) => (
    <TableRow key={instance.id}>
      <TableCell>
        <span className="block max-w-64 truncate">{instance.name}</span>
      </TableCell>
      <TableCell>
        <span className="font-mono">{instance.id}</span>
      </TableCell>
      <TableCell>
        <StateBadge state={instance.state} />
      </TableCell>
      <TableCell>{instance.type}</TableCell>
      <TableCell>
        <span className="font-mono tabular-nums">{instance.address}</span>
      </TableCell>
    </TableRow>
  ))
}

function TableSpecimen() {
  const [state, setState] = useState<TableState>("rows")

  return (
    <Specimen label="Table states">
      <div className="flex w-full flex-col gap-3">
        <div className="overflow-x-auto">
          <ToggleGroup
            onValueChange={(values) => {
              const next = values.at(-1)

              if (next) {
                setState(next)
              }
            }}
            value={[state]}
            variant="outline"
          >
            {TABLE_STATES.map((option) => (
              <ToggleGroupItem key={option} value={option}>
                {option}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
        </div>
        <div className="rounded-md border">
          <ScrollArea className="h-72">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Instance ID</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Type</TableHead>
                  <TableHead>Private IP</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                <InstanceRows state={state} />
              </TableBody>
            </Table>
          </ScrollArea>
        </div>
      </div>
    </Specimen>
  )
}

// Relative to the moment the page renders, so the specimen always reads the same.
const THIRTY_THREE_MINUTES_AGO = new Date(Date.now() - 33 * 60 * 1000)

const SAMPLE_TAGS = [
  { Key: "Name", Value: "web-1" },
  { Key: "spinifex:eks-cluster", Value: "test-cluster" },
  { Key: "spinifex:eks-nodegroup", Value: "node-gap" },
]

export function DataSection() {
  return (
    <CatalogueSection title="Data and layout">
      <TableSpecimen />
      <Specimen label="Table cells: relative time, truncated text, tag pills; hover for the full value">
        <RelativeTime date={THIRTY_THREE_MINUTES_AGO} />
        <TruncatedText text="a-very-long-instance-name-that-would-otherwise-widen-its-column" />
        <TagPills omit={["Name"]} tags={SAMPLE_TAGS} />
      </Specimen>
      <Specimen label="Breadcrumb">
        <Breadcrumb>
          <BreadcrumbList>
            <BreadcrumbItem>
              <BreadcrumbLink href="#">EC2</BreadcrumbLink>
            </BreadcrumbItem>
            <BreadcrumbSeparator />
            <BreadcrumbItem>
              <BreadcrumbLink href="#">Instances</BreadcrumbLink>
            </BreadcrumbItem>
            <BreadcrumbSeparator />
            <BreadcrumbItem>
              <BreadcrumbPage>i-0a1b2c3d4e5f67890</BreadcrumbPage>
            </BreadcrumbItem>
          </BreadcrumbList>
        </Breadcrumb>
      </Specimen>
      <Specimen label="Tabs and card">
        <Tabs className="w-full max-w-xl" defaultValue="overview">
          <TabsList>
            <TabsTab value="overview">Overview</TabsTab>
            <TabsTab value="networking">Networking</TabsTab>
            <TabsTab value="storage">Storage</TabsTab>
          </TabsList>
          <TabsPanel value="overview">
            <Card>
              <CardHeader>
                <CardTitle>Instance summary</CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-muted-foreground">
                  Cards hold one group of related facts. Use borders, not
                  shadows.
                </p>
              </CardContent>
            </Card>
          </TabsPanel>
          <TabsPanel value="networking">Networking panel</TabsPanel>
          <TabsPanel value="storage">Storage panel</TabsPanel>
        </Tabs>
      </Specimen>
      <Specimen label="Resizable panes">
        <div className="h-40 w-full rounded-md border">
          <ResizablePanelGroup>
            <ResizablePanel defaultSize="30%">
              <div className="p-3 text-muted-foreground">Filters</div>
            </ResizablePanel>
            <ResizableHandle withHandle />
            <ResizablePanel>
              <div className="p-3 text-muted-foreground">Table</div>
            </ResizablePanel>
            <ResizableHandle withHandle />
            <ResizablePanel defaultSize="30%">
              <div className="p-3 text-muted-foreground">Detail</div>
            </ResizablePanel>
          </ResizablePanelGroup>
        </div>
      </Specimen>
    </CatalogueSection>
  )
}
