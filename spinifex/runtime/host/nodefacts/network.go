package nodefacts

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
)

// ethtoolFacts is what `ethtool <if>` and `ethtool -i <if>` add to sysfs.
type ethtoolFacts struct {
	speedMbps int
	maxMbps   int
	duplex    string
	port      string
	driver    string
	firmware  string
	busInfo   string
}

var linkModeSpeed = regexp.MustCompile(`\b(\d+)base`)

// parseEthtool reads both outputs, concatenated in either order.
func parseEthtool(text string) ethtoolFacts {
	var e ethtoolFacts
	inSupported := false
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		key, value, hasColon := strings.Cut(trimmed, ":")
		value = strings.TrimSpace(value)
		if hasColon && !strings.Contains(key, "base") {
			inSupported = key == "Supported link modes"
		}
		if inSupported {
			for _, m := range linkModeSpeed.FindAllStringSubmatch(trimmed, -1) {
				if n, err := strconv.Atoi(m[1]); err == nil {
					e.maxMbps = max(e.maxMbps, n)
				}
			}
		}
		if !hasColon {
			continue
		}
		switch key {
		case "Speed":
			e.speedMbps = atoiPrefix(strings.TrimSuffix(value, "Mb/s"))
		case "Duplex":
			if value != "Unknown! (255)" {
				e.duplex = value
			}
		case "Port":
			if value != "Other" {
				e.port = value
			}
		case "driver":
			e.driver = value
		case "firmware-version":
			e.firmware = value
		case "bus-info":
			e.busInfo = value
		}
	}
	return e
}

// parseUdevModel reads the hwdb vendor and model from `udevadm info` output.
func parseUdevModel(text string) string {
	var vendor, model string
	for line := range strings.SplitSeq(text, "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "ID_VENDOR_FROM_DATABASE":
			vendor = value
		case "ID_MODEL_FROM_DATABASE":
			model = value
		}
	}
	vendor = strings.TrimSuffix(strings.TrimSuffix(vendor, " Corporation"), " Technologies")
	switch {
	case model == "":
		return vendor
	case vendor == "":
		return model
	}
	return vendor + " " + model
}

// lldpCapabilityNames are the IEEE 802.1AB system capability bits.
var lldpCapabilityNames = []string{"other", "repeater", "bridge", "wlan-ap", "router", "telephone", "docsis", "station"}

type networkctlLLDP struct {
	Neighbors []struct {
		InterfaceName string `json:"InterfaceName"`
		Neighbors     []struct {
			ChassisID           string `json:"ChassisID"`
			PortID              string `json:"PortID"`
			PortDescription     string `json:"PortDescription"`
			SystemName          string `json:"SystemName"`
			SystemDescription   string `json:"SystemDescription"`
			EnabledCapabilities int    `json:"EnabledCapabilities"`
		} `json:"Neighbors"`
	} `json:"Neighbors"`
}

// parseLLDP reads `networkctl lldp --json=short` into the first neighbour of
// each interface. A NIC sees one switch port, so a second is a hub or a loop.
func parseLLDP(data []byte) (map[string]clusterv1.LLDPNeighbor, error) {
	var doc networkctlLLDP
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := map[string]clusterv1.LLDPNeighbor{}
	for _, iface := range doc.Neighbors {
		if len(iface.Neighbors) == 0 {
			continue
		}
		n := iface.Neighbors[0]
		var caps []string
		for i, name := range lldpCapabilityNames {
			if n.EnabledCapabilities&(1<<i) != 0 {
				caps = append(caps, name)
			}
		}
		out[iface.InterfaceName] = clusterv1.LLDPNeighbor{
			ChassisID: n.ChassisID, SystemName: n.SystemName, SystemDescription: n.SystemDescription,
			PortID: n.PortID, PortDescription: n.PortDescription, Capabilities: caps,
		}
	}
	return out, nil
}
