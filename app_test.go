package main

import "testing"

func TestManagementRegistrationIncludesAllHandledRoutes(t *testing.T) {
	registration := managementRegistration().(map[string]any)
	routes := registration["routes"].([]map[string]any)
	registered := make(map[string]bool, len(routes))
	for _, route := range routes {
		registered[route["Method"].(string)+" "+route["Path"].(string)] = true
	}

	for _, expected := range []string{
		"GET /cpa-quota-estimator/summary",
		"GET /cpa-quota-estimator/series",
		"GET /cpa-quota-estimator/monthly",
		"GET /cpa-quota-estimator/prices",
		"POST /cpa-quota-estimator/prices/sync",
	} {
		if !registered[expected] {
			t.Fatalf("management route %q is handled but not registered", expected)
		}
	}
}
