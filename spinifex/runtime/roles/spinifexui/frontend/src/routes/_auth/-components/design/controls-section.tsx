import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"

import mariadbMark from "@/assets/engines/mariadb.svg?no-inline"
import postgresMark from "@/assets/engines/postgresql.svg?no-inline"
import { ChoiceTile, ChoiceTiles } from "@/components/choice-tile"
import { FilterBar } from "@/components/filter-bar"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Slider } from "@/components/ui/slider"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

import { CatalogueSection, Specimen } from "./catalogue-section"

const BUTTON_VARIANTS = [
  "default",
  "secondary",
  "outline",
  "ghost",
  "destructive",
  "link",
] as const

const BUTTON_SIZES = ["xs", "sm", "default", "lg"] as const

const BADGE_VARIANTS = [
  "default",
  "secondary",
  "outline",
  "destructive",
  "ghost",
] as const

const TIME_RANGES = ["1H", "1D", "1W", "1M"]

export function ControlsSection() {
  return (
    <CatalogueSection title="Controls">
      <Specimen label="Button variants">
        {BUTTON_VARIANTS.map((variant) => (
          <Button key={variant} variant={variant}>
            {variant}
          </Button>
        ))}
        <Button disabled>disabled</Button>
      </Specimen>
      <Specimen label="Button sizes and icons">
        {BUTTON_SIZES.map((size) => (
          <Button key={size} size={size}>
            <Plus data-icon="inline-start" />
            Create {size}
          </Button>
        ))}
        <Button aria-label="Delete" size="icon" variant="destructive">
          <Trash2 />
        </Button>
      </Specimen>
      <Specimen label="Badges">
        {BADGE_VARIANTS.map((variant) => (
          <Badge key={variant} variant={variant}>
            {variant}
          </Badge>
        ))}
      </Specimen>
      <Specimen label="Text inputs">
        <div className="grid w-full max-w-xl gap-3 sm:grid-cols-2">
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-name">Name</Label>
            <Input id="design-name" placeholder="my-vpc" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-cidr">CIDR block (invalid)</Label>
            <Input aria-invalid defaultValue="10.0.0.0/33" id="design-cidr" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-disabled">Disabled</Label>
            <Input disabled id="design-disabled" value="vpc-0a1b2c3d" />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="design-type">Instance type</Label>
            <Select defaultValue="t3.micro">
              <SelectTrigger className="w-full" id="design-type">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="t3.micro">t3.micro</SelectItem>
                <SelectItem value="t3.small">t3.small</SelectItem>
                <SelectItem value="t3.xlarge">t3.xlarge</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col gap-1.5 sm:col-span-2">
            <Label htmlFor="design-description">Description</Label>
            <Textarea
              id="design-description"
              placeholder="What this resource is for"
            />
          </div>
        </div>
      </Specimen>
      <Specimen label="Choices">
        <div className="flex items-center gap-2">
          <Switch defaultChecked id="design-switch" />
          <Label htmlFor="design-switch">Enable DNS hostnames</Label>
        </div>
        <div className="flex items-center gap-2">
          <Switch id="design-switch-off" />
          <Label htmlFor="design-switch-off">Auto-assign public IP</Label>
        </div>
        <div className="flex items-center gap-2">
          <Switch disabled id="design-switch-disabled" />
          <Label htmlFor="design-switch-disabled">Disabled</Label>
        </div>
        {/* A checkbox picks items from a list; a setting is a switch. */}
        <div className="flex items-center gap-2">
          <Checkbox defaultChecked id="design-check" />
          <Label htmlFor="design-check">sg-0a1b2c3d (web)</Label>
        </div>
        {/* One of a few exclusive choices in a form. */}
        <RadioGroup
          aria-label="Load balancer type"
          className="flex w-auto"
          defaultValue="application"
        >
          <Label>
            <RadioGroupItem value="application" />
            Application
          </Label>
          <Label>
            <RadioGroupItem value="network" />
            Network
          </Label>
        </RadioGroup>
        <ToggleGroup defaultValue={["1D"]} variant="outline">
          {TIME_RANGES.map((range) => (
            <ToggleGroupItem key={range} value={range}>
              {range}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        <div className="w-64">
          <Slider defaultValue={[20, 80]} />
        </div>
      </Specimen>
      <EngineTilesSpecimen />
      <FilterBarSpecimen />
    </CatalogueSection>
  )
}

function EngineTilesSpecimen() {
  const [engine, setEngine] = useState("mariadb")

  return (
    <Specimen label="Choice tiles: a few named products, each with its own mark; one that cannot be used stays visible, disabled, saying why">
      <div className="w-full max-w-md">
        <ChoiceTiles columns={3} label="Engine">
          <ChoiceTile
            checked={engine === "mariadb"}
            icon={<img alt="" className="size-8" src={mariadbMark} />}
            label="MariaDB"
            onSelect={() => {
              setEngine("mariadb")
            }}
          />
          <ChoiceTile
            checked={engine === "postgres"}
            icon={<img alt="" className="size-8" src={postgresMark} />}
            label="PostgreSQL"
            onSelect={() => {
              setEngine("postgres")
            }}
          />
          <ChoiceTile
            checked={false}
            description="Image not imported"
            icon={
              <img alt="" className="size-8 opacity-40" src={postgresMark} />
            }
            label="Unavailable"
          />
        </ChoiceTiles>
      </div>
    </Specimen>
  )
}

const FILTER_CHOICES = [
  { value: "all", label: "All", count: 12 },
  { value: "running", label: "Running", count: 9 },
  { value: "stopped", label: "Stopped", count: 3 },
] as const

type FilterChoiceValue = (typeof FILTER_CHOICES)[number]["value"]

function FilterBarSpecimen() {
  const [query, setQuery] = useState("")
  const [choice, setChoice] = useState<FilterChoiceValue>("all")

  return (
    <Specimen label="Filter bar: every list searches and filters with it; Clear filters shows only while something is filtered">
      <FilterBar
        choice={choice}
        choices={FILTER_CHOICES}
        choicesLabel="Instance state"
        onChoiceChange={setChoice}
        onQueryChange={setQuery}
        placeholder="Filter by name, ID, type or IP"
        query={query}
        searchLabel="Filter instances"
      />
    </Specimen>
  )
}
