// Command moose is the app-author's inner-loop CLI. v1 ships three `manifest`
// subcommands, runnable on a dev box with no brain:
//
//   - `lint` validates a manifest.yml against the schema (APP_MANIFEST.md) and
//     sanity-checks its sibling compose — the catalog CI schema-lint step
//     (APP_STORE.md # CI on the repo). Schema only; does NOT run admission. It
//     also runs the strict role and requires rules (manifest.Lint), which the
//     box itself never applies. `--ai-providers <path>` adds a check against
//     the store's ai_providers.yml.
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
// Output is for a human author: results go to stdout, errors and `warning:`
// lines to stderr, and a non-zero exit signals a failed lint. This is a CLI,
// not the brain daemon, so it prints rather than emitting structured slog. A
// warning alone does not fail the lint.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/manifest"
)

const usage = "usage:\n  moose manifest lint    [--ai-providers <path/to/ai_providers.yml>] <path/to/manifest.yml>\n  moose manifest check   [--ai-providers <path/to/ai_providers.yml>] <path/to/manifest.yml>\n  moose manifest resolve <path/to/manifest.yml>"

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

// run dispatches the subcommand: `manifest lint|check [--ai-providers <path>]
// <path>` or `manifest resolve <path>`; anything else is a usage error.
func run(args []string) error {
	if len(args) < 3 || args[0] != "manifest" {
		return errUsage
	}
	switch args[1] {
	case "lint", "check":
		path, providersPath, ok := lintArgs(args[2:])
		if !ok {
			return errUsage
		}
		var opts lintOptions
		if providersPath != "" {
			protocols, err := readNativeProtocols(providersPath)
			if err != nil {
				return err
			}
			opts.nativeProtocols = protocols
		}
		var (
			warnings []string
			err      error
		)
		if args[1] == "lint" {
			warnings, err = lint(path, opts)
		} else {
			warnings, err = check(context.Background(), admission.Check, path, opts)
		}
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "warning: "+w)
		}
		if err != nil {
			return err
		}
		if args[1] == "lint" {
			fmt.Printf("%s: ok\n", path)
		} else {
			fmt.Printf("%s: ok (schema + admission)\n", path)
		}
		return nil
	case "resolve":
		if len(args) != 3 {
			return errUsage
		}
		if err := resolve(context.Background(), dockerSizer{}, args[2]); err != nil {
			return err
		}
		fmt.Printf("%s: images resolved\n", args[2])
		return nil
	}
	return errUsage
}

// lintArgs reads `[--ai-providers <path>] <manifest>` in any order. ok is false
// for a missing, empty or extra argument.
func lintArgs(args []string) (path, providersPath string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--ai-providers":
			if i+1 >= len(args) || providersPath != "" {
				return "", "", false
			}
			i++
			providersPath = args[i]
			if providersPath == "" {
				return "", "", false
			}
		case strings.HasPrefix(a, "--ai-providers="):
			if providersPath != "" {
				return "", "", false
			}
			// An empty value (say, an unset CI variable) must not quietly
			// switch the provider check off.
			providersPath = strings.TrimPrefix(a, "--ai-providers=")
			if providersPath == "" {
				return "", "", false
			}
		case path == "" && !strings.HasPrefix(a, "-"):
			path = a
		default:
			return "", "", false
		}
	}
	return path, providersPath, path != ""
}

// lintOptions are the optional inputs of lint and check.
type lintOptions struct {
	// nativeProtocols is the set of native protocols the store's providers
	// offer, read from --ai-providers. nil skips that check.
	nativeProtocols map[string]bool
}

// readNativeProtocols reads the store's ai_providers.yml and returns the
// native protocols its providers offer. The list is under `providers:` in the
// store's file and under `ai_providers:` in the published snapshot shape; both
// are read. It reads only what it needs and skips what it cannot read: the
// store's own publisher is the strict gate for that file. A file with no
// provider at all is an error, so a wrong path does not turn into a warning on
// every native slot.
func readNativeProtocols(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ai providers: %w", err)
	}
	var doc struct {
		Providers   []yaml.Node `yaml:"providers"`
		AIProviders []yaml.Node `yaml:"ai_providers"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("ai providers %s: %w", path, err)
	}
	nodes := append(doc.Providers, doc.AIProviders...)
	if len(nodes) == 0 {
		return nil, fmt.Errorf("ai providers %s: no providers: or ai_providers: list found", path)
	}
	out := map[string]bool{}
	for _, node := range nodes {
		var p struct {
			NativeProtocol string `yaml:"native_protocol"`
		}
		if node.Decode(&p) == nil && p.NativeProtocol != "" {
			out[p.NativeProtocol] = true
		}
	}
	return out, nil
}

// lint parses the manifest at path and cross-checks its compose file: the
// manifest validates against the schema (manifest.Parse), the compose_file
// (resolved relative to the manifest) exists and parses as YAML, and
// main_service is one of the services it declares. The returned error is
// author-actionable and names the problem.
//
// It then runs the strict role and requires rules (manifest.Lint). Their
// errors fail the lint, all listed at once; their warnings are returned for the
// caller to print and never fail it.
func lint(path string, opts lintOptions) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	man, err := manifest.Parse(data)
	if err != nil {
		return nil, err // manifest.Parse errors already name the field/slug/permission at fault
	}
	if err := lintCompose(path, man); err != nil {
		return nil, err
	}
	errs, warnings := manifest.Lint(man, manifest.LintOptions{NativeProtocols: opts.nativeProtocols})
	if len(errs) > 0 {
		return warnings, fmt.Errorf("%d problem(s) with roles or requires:\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	return warnings, nil
}

// lintCompose checks the compose_file resolves, parses, and declares
// main_service.
func lintCompose(path string, man *manifest.Manifest) error {
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
