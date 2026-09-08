package main

import (
	"os"
	"strings"
	"testing"

	"avito-kitchen/openapi"
)

func TestCheckContract(t *testing.T) {
	source, err := os.ReadFile("../../internal/httpapi/api.go")
	if err != nil {
		t.Fatal(err)
	}
	spec := string(openapi.Spec)
	for _, tc := range []struct {
		name, spec, source string
		wantError          bool
	}{
		{"valid", spec, string(source), false},
		{"invalid_schema", strings.Replace(spec, "type: object", "type: invalid", 1), string(source), true},
		{"broken_reference", strings.Replace(spec, "#/components/schemas/Health", "#/components/schemas/Missing", 1), string(source), true},
		{"undocumented_route", spec, strings.Replace(string(source), "GET /healthz", "GET /undocumented", 1), true},
		{"unimplemented_route", spec, strings.Replace(string(source), "mux.HandleFunc(\"GET /healthz\", a.wrap(a.health))", "", 1), true},
		{"missing_partner_security", strings.Replace(spec, "- PartnerKey: []", "- {}", 1), string(source), true},
		{"missing_gateway_security", strings.Replace(spec, "- GatewayKey: []", "- {}", 1), string(source), true},
		{"wrong_partner_scheme", strings.Replace(spec, "scheme: bearer", "scheme: basic", 1), string(source), true},
		{"wrong_gateway_header", strings.Replace(spec, "name: X-Gateway-Key", "name: X-Wrong-Key", 1), string(source), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count, err := check([]byte(tc.spec), []byte(tc.source))
			if (err != nil) != tc.wantError {
				t.Fatalf("check error = %v; want error: %v", err, tc.wantError)
			}
			if !tc.wantError && count != 17 {
				t.Fatalf("got %d routes, want 17", count)
			}
		})
	}
}
