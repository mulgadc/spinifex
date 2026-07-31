package handlers_rds

// PostgreSQL 18 is the pinned v1 major, matching the rds-postgres AMI preset.
// A new major is a new AMI plus a bump here, never a runtime upgrade.
var enginePostgres = Engine{
	Name:         "postgres",
	MajorVersion: "18",
	DefaultPort:  5432,
	// rdsadmin is the management role AWS reserves; postgres is the cluster
	// superuser initdb creates, which the master role must not collide with.
	reservedUsernames:        []string{"rdsadmin", "postgres"},
	reservedUsernamePrefixes: []string{"pg_"},
}
