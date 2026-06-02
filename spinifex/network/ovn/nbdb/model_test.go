package nbdb

import (
	"testing"
)

func TestFullDatabaseModel(t *testing.T) {
	dbModel, err := FullDatabaseModel()
	if err != nil {
		t.Fatalf("FullDatabaseModel() returned error: %v", err)
	}

	if dbModel.Name() != "OVN_Northbound" {
		t.Errorf("database name = %q, want %q", dbModel.Name(), "OVN_Northbound")
	}

	expectedTables := []string{
		"Logical_Switch",
		"Logical_Switch_Port",
		"Logical_Router",
		"Logical_Router_Port",
		"DHCP_Options",
		"NAT",
		"Logical_Router_Static_Route",
		"Logical_Router_Policy",
		"Gateway_Chassis",
		"Port_Group",
		"ACL",
		"Address_Set",
	}

	types := dbModel.Types()
	for _, table := range expectedTables {
		if _, ok := types[table]; !ok {
			t.Errorf("missing table %q in database model", table)
		}
	}

	if len(types) != len(expectedTables) {
		t.Errorf("table count = %d, want %d", len(types), len(expectedTables))
	}
}
