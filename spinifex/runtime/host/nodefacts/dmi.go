package nodefacts

import (
	"slices"
	"strconv"
	"strings"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
)

// dmiBlock is one structure from dmidecode's text output: its title line and
// its "Key: value" fields. Indented list items are folded under their key.
type dmiBlock struct {
	title  string
	fields map[string]string
	lists  map[string][]string
}

func (b dmiBlock) get(key string) string {
	return cleanDMI(b.fields[key])
}

// parseDMI splits dmidecode output into its structures, in order.
func parseDMI(text string) []dmiBlock {
	var (
		blocks  []dmiBlock
		cur     *dmiBlock
		listKey string
	)
	flush := func() {
		if cur != nil {
			blocks = append(blocks, *cur)
			cur = nil
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, " \r")
		switch {
		case line == "" || strings.HasPrefix(line, "Handle ") || strings.HasPrefix(line, "#"):
			flush()
			listKey = ""
		case !strings.HasPrefix(line, "\t"):
			if strings.HasPrefix(line, "SMBIOS ") || strings.HasPrefix(line, "Getting SMBIOS") {
				continue
			}
			flush()
			cur = &dmiBlock{title: line, fields: map[string]string{}, lists: map[string][]string{}}
			listKey = ""
		case cur == nil:
		case strings.HasPrefix(line, "\t\t"):
			if listKey != "" {
				cur.lists[listKey] = append(cur.lists[listKey], strings.TrimSpace(line))
			}
		default:
			key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			cur.fields[key] = value
			listKey = ""
			if value == "" {
				listKey = key
			}
		}
	}
	flush()
	return blocks
}

func blocksTitled(blocks []dmiBlock, title string) []dmiBlock {
	var out []dmiBlock
	for _, b := range blocks {
		if b.title == title {
			out = append(out, b)
		}
	}
	return out
}

// placeholders are the values firmware writes when a field was never filled.
var placeholders = map[string]bool{
	"": true, "unknown": true, "not specified": true, "not present": true, "none": true,
	"to be filled by o.e.m.": true, "default string": true, "system serial number": true,
	"system product name": true, "system manufacturer": true, "o.e.m.": true, "oem": true,
	"0123456789": true, "123456789": true, "no asset tag": true, "chassis serial number": true,
	"base board serial number": true, "not settable": true, "unknown unknown": true,
	"03000200-0400-0500-0006-000700080009": true,
}

// cleanDMI trims a DMI value and drops the placeholders firmware ships with.
func cleanDMI(v string) string {
	v = strings.TrimSpace(v)
	lower := strings.ToLower(v)
	if placeholders[lower] || strings.HasSuffix(lower, "_assettag") || strings.Trim(lower, "0") == "" {
		return ""
	}
	return v
}

// parseIdentity reads the bios, system, chassis and baseboard structures.
func parseIdentity(text string) clusterv1.SystemFacts {
	blocks := parseDMI(text)
	var s clusterv1.SystemFacts
	if b := blocksTitled(blocks, "BIOS Information"); len(b) > 0 {
		s.BIOSVendor, s.BIOSVersion, s.BIOSDate = b[0].get("Vendor"), b[0].get("Version"), b[0].get("Release Date")
	}
	if b := blocksTitled(blocks, "System Information"); len(b) > 0 {
		s.Vendor, s.Product, s.Family = b[0].get("Manufacturer"), b[0].get("Product Name"), b[0].get("Family")
		s.Version, s.Serial, s.UUID = b[0].get("Version"), b[0].get("Serial Number"), b[0].get("UUID")
	}
	if b := blocksTitled(blocks, "Chassis Information"); len(b) > 0 {
		s.ChassisType, s.ChassisAsset = b[0].get("Type"), b[0].get("Asset Tag")
	}
	if b := blocksTitled(blocks, "Base Board Information"); len(b) > 0 {
		s.BoardVendor, s.BoardProduct, s.BoardSerial = b[0].get("Manufacturer"), b[0].get("Product Name"), b[0].get("Serial Number")
	}
	return s
}

// parseProcessors summarises the populated sockets.
func parseProcessors(text string) clusterv1.CPUFacts {
	var c clusterv1.CPUFacts
	for _, b := range blocksTitled(parseDMI(text), "Processor Information") {
		if !strings.HasPrefix(b.fields["Status"], "Populated") {
			continue
		}
		c.Sockets++
		if c.Model == "" {
			c.Model = strings.Join(strings.Fields(b.get("Version")), " ")
			c.Vendor = b.get("Manufacturer")
		}
		cores, _ := strconv.Atoi(b.fields["Core Count"])
		threads, _ := strconv.Atoi(b.fields["Thread Count"])
		c.Cores += cores
		c.Threads += threads
		c.CoresPerSocket = max(c.CoresPerSocket, cores)
		c.MaxMHz = max(c.MaxMHz, atoiPrefix(b.fields["Max Speed"]))
		c.CurrentMHz = max(c.CurrentMHz, atoiPrefix(b.fields["Current Speed"]))
	}
	return c
}

// memoryKey is everything a DIMM is grouped by.
type memoryKey struct {
	size                                 uint64
	typ, detail, form, maker, part, rank string
	speed, configured                    int
}

// parseMemory groups the populated DIMMs and counts the slots of every array.
func parseMemory(text string) clusterv1.MemoryFacts {
	var m clusterv1.MemoryFacts
	blocks := parseDMI(text)
	for _, b := range blocksTitled(blocks, "Physical Memory Array") {
		if b.fields["Use"] != "System Memory" {
			continue
		}
		if m.MaxCapacity == "" {
			m.MaxCapacity = b.get("Maximum Capacity")
		}
		if m.ECC == "" {
			m.ECC = b.get("Error Correction Type")
		}
	}

	groups := map[memoryKey]*clusterv1.MemoryGroup{}
	var order []memoryKey
	for _, b := range blocksTitled(blocks, "Memory Device") {
		m.SlotsTotal++
		size := parseSize(b.fields["Size"])
		if size == 0 {
			continue
		}
		m.SlotsUsed++
		m.TotalBytes += size
		k := memoryKey{
			size: size, typ: b.get("Type"), detail: b.get("Type Detail"), form: b.get("Form Factor"),
			maker: b.get("Manufacturer"), part: b.get("Part Number"), rank: b.get("Rank"),
			speed: atoiPrefix(b.fields["Speed"]), configured: atoiPrefix(b.fields["Configured Memory Speed"]),
		}
		g, ok := groups[k]
		if !ok {
			g = &clusterv1.MemoryGroup{
				SizeBytes: size, Type: k.typ, TypeDetail: k.detail, FormFactor: k.form,
				SpeedMTs: k.speed, ConfiguredMTs: k.configured, Manufacturer: k.maker,
				PartNumber: k.part, Rank: k.rank,
			}
			groups[k] = g
			order = append(order, k)
		}
		g.Count++
		if serial := b.get("Serial Number"); serial != "" {
			g.Serials = append(g.Serials, serial)
		}
	}
	for _, k := range order {
		m.Groups = append(m.Groups, *groups[k])
	}
	slices.SortStableFunc(m.Groups, func(a, b clusterv1.MemoryGroup) int { return b.Count - a.Count })
	return m
}

// parseSize reads a DMI size such as "16 GB" or "4096 MB"; anything else,
// including "No Module Installed", is zero.
func parseSize(v string) uint64 {
	fields := strings.Fields(v)
	if len(fields) != 2 {
		return 0
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	switch fields[1] {
	case "kB", "KB":
		return n << 10
	case "MB":
		return n << 20
	case "GB":
		return n << 30
	case "TB":
		return n << 40
	}
	return 0
}

// atoiPrefix reads the leading integer of a value such as "3675 MHz".
func atoiPrefix(v string) int {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(fields[0])
	return n
}
