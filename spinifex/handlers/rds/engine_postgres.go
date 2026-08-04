package handlers_rds

// PostgreSQL 18 is the pinned v1 major, matching the rds-postgres AMI preset.
// A new major is a new AMI plus a bump here, never a runtime upgrade.
var enginePostgres = Engine{
	Name:         "postgres",
	MajorVersion: "18",
	DefaultPort:  5432,
	// rdsadmin is the management role AWS reserves; postgres is the cluster
	// superuser initdb creates, which the master role must not collide with.
	// rds_superuser is the group role the bootstrap grants the master its
	// administrative privileges through.
	reservedUsernames:        []string{"rdsadmin", "postgres", "rds_superuser"},
	reservedUsernamePrefixes: []string{"pg_"},
}

// The pinned engine as a value, for the in-guest agent: it enforces the same
// reserved-role set before altering a role as the cluster superuser, and a
// lookup by name there could only fail silently into an empty set.
func EnginePostgres() Engine { return enginePostgres }
