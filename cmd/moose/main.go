// Command moose is the app-author's inner-loop CLI. v1 ships three `manifest`
// subcommands, runnable on a dev box with no brain:
//
//   - `lint` validates a manifest.yml against the schema (APP_MANIFEST.md) and
//     sanity-checks its sibling compose — the catalog CI schema-lint step
//     (APP_STORE.md # CI on the repo). Schema only; does NOT run admission.
//   - `check` runs `lint` AND the compose admission policy (admission.Check,
//     APP_LIFECYCLE.md) in one pass — the "would this actually install?" gate.
//     A single green `check` proves both the schema and the structural compose
//     rules, so authors never hand-eyeball the admission rules.
//   - `resolve` fills the manifest's `images` block with registry-resolved
//     digests and download/disk sizes (APP_STORE.md # Catalog schema), driving
//     the local Docker daemon — the catalog CI digest/size-resolution step.
//     Unlike lint/check, this MUTATES the manifest in place.
//
// Other dev subcommands (`install --local`, …) are deferred (NEXT.md #
// Developer / app-author surface).
//
// Output is for a human author: results go to stdout, errors to stderr, and a
// non-zero exit signals a failed lint — this is a CLI, not the brain daemon, so
// it prints rather than emitting structured slog.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/manifest"
)

const usage = "usage:\n  moose manifest lint    <path/to/manifest.yml>\n  moose manifest check   <path/to/manifest.yml>\n  moose manifest resolve <path/to/manifest.yml>"

// errUsage signals a malformed invocation (wrong/missing subcommand or args),
// as opposed to a lint failure. It maps to exit 2 (Unix convention for usage
// errors); a failed lint is exit 1.
var errUsage = errors.New(usage)

func main() {
	switch err := run(os.Args[1:]); {
	case err == nil:
		return
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "moose: "+err.Error())
		os.Exit(1)
	}
}

// run dispatches the subcommand: `manifest lint <path>` or `manifest resolve
// <path>`; anything else is a usage error.
func run(args []string) error {
	if len(args) == 3 && args[0] == "manifest" {
		path := args[2]
		switch args[1] {
		case "lint":
			if err := lint(path); err != nil {
				return err
			}
			fmt.Printf("%s: ok\n", path)
			return nil
		case "check":
			if err := check(context.Background(), admission.Check, path); err != nil {
				return err
			}
			fmt.Printf("%s: ok (schema + admission)\n", path)
			return nil
		case "resolve":
			if err := resolve(context.Background(), dockerSizer{}, path); err != nil {
				return err
			}
			fmt.Printf("%s: images resolved\n", path)
			return nil
		}
	}
	return errUsage
}

// lint parses the manifest at path and cross-checks its compose file: the
// manifest validates against the schema (manifest.Parse), the compose_file
// (resolved relative to the manifest) exists and parses as YAML, and
// main_service is one of the services it declares. The returned error is
// author-actionable and names the problem.
func lint(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	man, err := manifest.Parse(data)
	if err != nil {
		return err // manifest.Parse errors already name the field/slug/permission at fault
	}

	composePath := filepath.Join(filepath.Dir(path), man.ComposeFile)
	composeData, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("compose_file %q: %w", man.ComposeFile, err)
	}
	services, err := manifest.ComposeServiceNames(composeData)
	if err != nil {
		return fmt.Errorf("compose_file %q: %w", man.ComposeFile, err)
	}
	if !slices.Contains(services, man.MainService) {
		return fmt.Errorf("main_service %q is not a service in %s (declared services: %s)",
			man.MainService, man.ComposeFile, strings.Join(services, ", "))
	}
	return nil
}
