// Command openapi-gen emits the brain's OpenAPI 3 spec to JSON + YAML without
// starting a server or opening a port. It is the build-time spec emitter behind
// `make openapi` (regenerate the committed api/openapi.{json,yaml}) and
// `make openapi-check` (the freshness gate run in CI).
//
// The spec is generated from the huma handler registrations
// (api.OpenAPIDocument), so the committed artifact can never drift from the
// routes the brain actually serves — the schema is a byproduct of the code, not
// a hand-maintained file (BRAIN_UI_PROTOCOL.md # Codegen / # Why generated, not
// hand-written OpenAPI). huma marshals with stable field ordering and Go sorts
// map keys, so the output is byte-stable across runs, which is what makes the
// freshness check meaningful.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onmoose/moose/internal/api"
)

func main() {
	outDir := flag.String("o", "api", "directory to write openapi.json and openapi.yaml into")
	flag.Parse()

	if err := emit(*outDir); err != nil {
		fmt.Fprintln(os.Stderr, "openapi-gen: "+err.Error())
		os.Exit(1)
	}
}

// emit serializes the spec to <outDir>/openapi.json and <outDir>/openapi.yaml.
func emit(outDir string) error {
	doc := api.OpenAPIDocument()

	jsonBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	jsonBytes = append(jsonBytes, '\n') // keep the file POSIX-clean (trailing newline)

	yamlBytes, err := doc.YAML()
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "openapi.json"), jsonBytes, 0o644); err != nil {
		return fmt.Errorf("write openapi.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "openapi.yaml"), yamlBytes, 0o644); err != nil {
		return fmt.Errorf("write openapi.yaml: %w", err)
	}
	return nil
}
