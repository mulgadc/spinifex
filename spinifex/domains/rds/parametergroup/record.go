// Package parametergroup owns the RDS DB parameter group: its durable record, the stored overrides
// that sit alongside it, and the in-use guard on its delete. Default groups are never stored; the
// engine catalogue is their only source.
package parametergroup

import "time"

const prefix = "db-parameter-groups/"

// Prefix returns the per-account key prefix under which every DB parameter group's keys live.
// A group's own record is at .../meta and its values hang off .../params/, so listing groups walks
// the meta keys.
func Prefix() string {
	return prefix
}

// MetaKey returns the per-account KV key of a DB parameter group's own record.
func MetaKey(name string) string {
	return prefix + name + "/meta"
}

// ParamsPrefix returns the prefix holding one key per parameter value rather than one blob, so a
// modify touching a single parameter cannot clobber a concurrent change to another.
func ParamsPrefix(name string) string {
	return prefix + name + "/params/"
}

// ParamKey returns the per-account KV key of one parameter override in a DB parameter group.
func ParamKey(name, param string) string {
	return ParamsPrefix(name) + param
}

// Record is the db-parameter-groups/{name}/meta record. The values themselves live one key each
// under .../params/, so a modify touching one parameter cannot clobber a concurrent change to
// another. A default group, synthesised rather than stored, carries no tags and no CreatedAt.
type Record struct {
	Name        string `json:"name"`
	AccountID   string `json:"accountId"`
	Family      string `json:"family"`
	Description string `json:"description"`

	Tags map[string]string `json:"tags,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (r *Record) GetTags() map[string]string { return r.Tags }

func (r *Record) SetTags(tags map[string]string) { r.Tags = tags }

// Override is one stored value, at db-parameter-groups/{name}/params/{key}. ApplyMethod is the
// customer's request rather than a fact: whether a change lands live is decided by the parameter's
// own ApplyType.
type Override struct {
	Name        string    `json:"name"`
	Value       string    `json:"value"`
	ApplyMethod string    `json:"applyMethod,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
