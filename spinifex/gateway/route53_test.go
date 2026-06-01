package gateway

import "testing"

func TestRoute53Routes(t *testing.T) {
	cases := []struct {
		method     string
		path       string
		wantAction string
		wantParams []string
		wantOK     bool
	}{
		{"POST", "/2013-04-01/hostedzone", "CreateHostedZone", nil, true},
		{"GET", "/2013-04-01/hostedzone", "ListHostedZones", nil, true},
		{"GET", "/2013-04-01/hostedzone/Z123ABC456DEF", "GetHostedZone", []string{"Z123ABC456DEF"}, true},
		{"POST", "/2013-04-01/hostedzone/Z123ABC456DEF", "UpdateHostedZoneComment", []string{"Z123ABC456DEF"}, true},
		{"DELETE", "/2013-04-01/hostedzone/Z123ABC456DEF", "DeleteHostedZone", []string{"Z123ABC456DEF"}, true},
		{"POST", "/2013-04-01/hostedzone/Z123ABC456DEF/rrset", "ChangeResourceRecordSets", []string{"Z123ABC456DEF"}, true},
		{"POST", "/2013-04-01/hostedzone/Z123ABC456DEF/rrset/", "ChangeResourceRecordSets", []string{"Z123ABC456DEF"}, true},
		{"GET", "/2013-04-01/hostedzone/Z123ABC456DEF/rrset", "ListResourceRecordSets", []string{"Z123ABC456DEF"}, true},
		{"GET", "/2013-04-01/change/C123ABC", "GetChange", []string{"C123ABC"}, true},

		// Wrong prefix → not matched (would fall through to InvalidAction).
		{"GET", "/hostedzone", "", nil, false},
		// Wrong method → not matched.
		{"PATCH", "/2013-04-01/hostedzone", "", nil, false},
		// Unknown sub-resource → not matched.
		{"POST", "/2013-04-01/garbage", "", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			action, params, handler, ok := lookupRoute53Action(tc.method, tc.path)
			if ok != tc.wantOK {
				t.Fatalf("lookupRoute53Action ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if action != tc.wantAction {
				t.Errorf("action = %q, want %q", action, tc.wantAction)
			}
			if len(params) != len(tc.wantParams) {
				t.Fatalf("params len = %d, want %d (got %v)", len(params), len(tc.wantParams), params)
			}
			for i, p := range params {
				if p != tc.wantParams[i] {
					t.Errorf("params[%d] = %q, want %q", i, p, tc.wantParams[i])
				}
			}
			if handler == nil {
				t.Errorf("handler is nil")
			}
		})
	}
}
