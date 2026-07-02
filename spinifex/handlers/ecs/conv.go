package handlers_ecs

import (
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// awsStringSlice dereferences a []*string, dropping nils/empties.
func awsStringSlice(in []*string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if v := aws.StringValue(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// clusterShortName extracts the cluster name from a name or a cluster ARN.
func clusterShortName(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return defaultCluster
	}
	if i := strings.LastIndex(ref, "cluster/"); i >= 0 {
		return ref[i+len("cluster/"):]
	}
	return ref
}

// containerInstanceShortID extracts the container-instance UUID from an ID or ARN.
func containerInstanceShortID(ref string) string {
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// taskShortID extracts the task UUID from a bare id or a task ARN
// (arn:...:task/{cluster}/{taskID}); the final path segment is the task id.
func taskShortID(ref string) string {
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// containerDefsFromAWS maps SDK container definitions to the persisted subset.
func containerDefsFromAWS(in []*ecs.ContainerDefinition) []ContainerDef {
	out := make([]ContainerDef, 0, len(in))
	for _, c := range in {
		if c == nil {
			continue
		}
		def := ContainerDef{
			Name:      aws.StringValue(c.Name),
			Image:     aws.StringValue(c.Image),
			CPU:       int(aws.Int64Value(c.Cpu)),
			MemoryMiB: int(aws.Int64Value(c.Memory)),
			Essential: aws.BoolValue(c.Essential),
			Command:   awsStringSlice(c.Command),
		}
		for _, e := range c.Environment {
			if e == nil {
				continue
			}
			if def.Environment == nil {
				def.Environment = map[string]string{}
			}
			def.Environment[aws.StringValue(e.Name)] = aws.StringValue(e.Value)
		}
		for _, p := range c.PortMappings {
			if p == nil {
				continue
			}
			def.PortMappings = append(def.PortMappings, bus.PortMapping{
				ContainerPort: int(aws.Int64Value(p.ContainerPort)),
				HostPort:      int(aws.Int64Value(p.HostPort)),
				Protocol:      aws.StringValue(p.Protocol),
			})
		}
		if lc := c.LogConfiguration; lc != nil {
			def.LogDriver = aws.StringValue(lc.LogDriver)
			for k, v := range lc.Options {
				if def.LogOptions == nil {
					def.LogOptions = map[string]string{}
				}
				def.LogOptions[k] = aws.StringValue(v)
			}
		}
		out = append(out, def)
	}
	return out
}

func (c ContainerDef) toAWS() *ecs.ContainerDefinition {
	cd := &ecs.ContainerDefinition{
		Name:      aws.String(c.Name),
		Image:     aws.String(c.Image),
		Essential: aws.Bool(c.Essential),
	}
	if c.CPU > 0 {
		cd.Cpu = aws.Int64(int64(c.CPU))
	}
	if c.MemoryMiB > 0 {
		cd.Memory = aws.Int64(int64(c.MemoryMiB))
	}
	for _, cmd := range c.Command {
		cd.Command = append(cd.Command, aws.String(cmd))
	}
	for k, v := range c.Environment {
		cd.Environment = append(cd.Environment, &ecs.KeyValuePair{Name: aws.String(k), Value: aws.String(v)})
	}
	for _, p := range c.PortMappings {
		pm := &ecs.PortMapping{ContainerPort: aws.Int64(int64(p.ContainerPort))}
		if p.HostPort > 0 {
			pm.HostPort = aws.Int64(int64(p.HostPort))
		}
		if p.Protocol != "" {
			pm.Protocol = aws.String(p.Protocol)
		}
		cd.PortMappings = append(cd.PortMappings, pm)
	}
	if c.LogDriver != "" {
		lc := &ecs.LogConfiguration{LogDriver: aws.String(c.LogDriver)}
		if len(c.LogOptions) > 0 {
			lc.Options = map[string]*string{}
			for k, v := range c.LogOptions {
				lc.Options[k] = aws.String(v)
			}
		}
		cd.LogConfiguration = lc
	}
	return cd
}

// toAssignContainer maps a persisted container def to its bus assign payload.
func (c ContainerDef) toAssignContainer() bus.AssignContainer {
	return bus.AssignContainer{
		Name:         c.Name,
		Image:        c.Image,
		CPU:          c.CPU,
		MemoryMiB:    c.MemoryMiB,
		Essential:    c.Essential,
		Command:      c.Command,
		Environment:  c.Environment,
		PortMappings: c.PortMappings,
		LogDriver:    c.LogDriver,
	}
}
