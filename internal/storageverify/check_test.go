package storageverify

import (
	"os"
	"path/filepath"
	"testing"
)

func newCfg(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"etc/moose", "srv/moose", "var/lib/moose"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return Config{
		Root:                root,
		MarkerPath:          "/etc/moose/data-drive.enrolled",
		DataDriveCanaryPath: "/srv/moose/.canary",
		BindMountCanaryPath: "/var/lib/moose/.canary",
	}
}

func writeFile(t *testing.T, cfg Config, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg.Root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheck_NoMarker_LevelZeroHealthy(t *testing.T) {
	cfg := newCfg(t)

	findings := Check(cfg)
	if len(findings) != 0 {
		t.Errorf("Level-0 boot (no marker) must be empty findings, got %v", findings)
	}
}

func TestCheck_MarkerAndCanariesMatch_Healthy(t *testing.T) {
	cfg := newCfg(t)
	writeFile(t, cfg, "etc/moose/data-drive.enrolled", `{"uuid":"abc-123","enrolled_at":"2026-04-12T08:00:00Z"}`)
	writeFile(t, cfg, "srv/moose/.canary", "abc-123\n")
	writeFile(t, cfg, "var/lib/moose/.canary", "abc-123\n")

	findings := Check(cfg)
	if len(findings) != 0 {
		t.Errorf("matching marker + canaries must be empty findings, got %v", findings)
	}
}

func TestCheck_MarkerPresent_DataCanaryAbsent_DataDriveMissing(t *testing.T) {
	cfg := newCfg(t)
	writeFile(t, cfg, "etc/moose/data-drive.enrolled", `{"uuid":"abc-123","enrolled_at":"2026-04-12T08:00:00Z"}`)

	findings := Check(cfg)
	if len(findings) != 1 || findings[0].ID != "data-drive-missing" {
		t.Fatalf("want data-drive-missing, got %v", findings)
	}
}

func TestCheck_CanaryUUIDDoesNotMatchMarker_DataDriveWrong(t *testing.T) {
	cfg := newCfg(t)
	writeFile(t, cfg, "etc/moose/data-drive.enrolled", `{"uuid":"abc-123","enrolled_at":"2026-04-12T08:00:00Z"}`)
	writeFile(t, cfg, "srv/moose/.canary", "xyz-999\n")

	findings := Check(cfg)
	if len(findings) != 1 || findings[0].ID != "data-drive-wrong" {
		t.Fatalf("want data-drive-wrong, got %v", findings)
	}
}

func TestCheck_BindCanaryMissing_CanaryMismatch(t *testing.T) {
	cfg := newCfg(t)
	writeFile(t, cfg, "etc/moose/data-drive.enrolled", `{"uuid":"abc-123","enrolled_at":"2026-04-12T08:00:00Z"}`)
	writeFile(t, cfg, "srv/moose/.canary", "abc-123\n")
	// no bind canary written

	findings := Check(cfg)
	if len(findings) != 1 || findings[0].ID != "canary-mismatch" {
		t.Fatalf("want canary-mismatch, got %v", findings)
	}
}

func TestCheck_BindCanaryDifferentFromDataCanary_CanaryMismatch(t *testing.T) {
	cfg := newCfg(t)
	writeFile(t, cfg, "etc/moose/data-drive.enrolled", `{"uuid":"abc-123","enrolled_at":"2026-04-12T08:00:00Z"}`)
	writeFile(t, cfg, "srv/moose/.canary", "abc-123\n")
	writeFile(t, cfg, "var/lib/moose/.canary", "stale-uuid\n") // bind landed on wrong fs

	findings := Check(cfg)
	if len(findings) != 1 || findings[0].ID != "canary-mismatch" {
		t.Fatalf("want canary-mismatch, got %v", findings)
	}
}

func TestCheck_MalformedMarker_HealthReportMalformed(t *testing.T) {
	cfg := newCfg(t)
	writeFile(t, cfg, "etc/moose/data-drive.enrolled", "{not json")

	findings := Check(cfg)
	if len(findings) != 1 || findings[0].ID != "health-report-malformed" {
		t.Fatalf("want health-report-malformed, got %v", findings)
	}
}

func TestCheck_EmptyUUIDInMarker_HealthReportMalformed(t *testing.T) {
	cfg := newCfg(t)
	writeFile(t, cfg, "etc/moose/data-drive.enrolled", `{"enrolled_at":"2026-04-12T08:00:00Z"}`)

	findings := Check(cfg)
	if len(findings) != 1 || findings[0].ID != "health-report-malformed" {
		t.Fatalf("want health-report-malformed, got %v", findings)
	}
}
