package main

import (
	"os"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestOpenAPIDocumentParsesAndContainsImplementedPaths(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read OpenAPI document: %v", err)
	}
	var document struct {
		OpenAPI string                    `yaml:"openapi"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse OpenAPI document: %v", err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("OpenAPI version = %q", document.OpenAPI)
	}
	for _, path := range []string{
		"/health",
		"/ready",
		"/v1/loyalty/wallet",
		"/v1/loyalty/transactions",
		"/v1/loyalty/rewards",
		"/internal/v1/loyalty/reservations",
		"/internal/v1/loyalty/reservations/{reservation_id}/release",
		"/internal/v1/loyalty/reservations/{reservation_id}/capture",
	} {
		if _, ok := document.Paths[path]; !ok {
			t.Errorf("implemented path %q is absent from OpenAPI", path)
		}
	}
}
