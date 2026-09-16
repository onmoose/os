package relmanifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// cacheFile is the cached manifest's name inside the state dir
// (RELEASE_MANIFEST.md # Failure modes names /var/lib/moose/manifest.json).
const cacheFile = "manifest.json"

// ManifestPath is the cached manifest's path inside the state dir.
func ManifestPath(dir string) string { return filepath.Join(dir, cacheFile) }

// cacheEnvelope is what actually sits in that file: the publisher's manifest
// bytes and the signature over them, together.
//
// **One file, not two, and the reason is a crash.** The obvious layout is
// manifest.json beside manifest.json.minisig, and it cannot keep the promise
// RELEASE_MANIFEST.md # Failure modes makes — "the previous valid manifest
// stays in effect". Two files means two renames. Lose power between them and
// the box comes back with the new manifest beside the old signature: that pair
// fails verification, and the previous good manifest is already gone, because
// the first rename overwrote it. The box then has no usable cache at all, which
// is worse than the failure the spec was describing. One file is one rename, so
// the box always comes back to a complete pair — the new one or the old one.
//
// Manifest holds the publisher's exact bytes, as a JSON string. They are not
// re-marshalled through our struct: that would drop the unknown fields Parse
// ignores and reorder the rest, and the signature covers the exact bytes, so
// the round trip would produce something that can never verify again.
type cacheEnvelope struct {
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
}

// Cached is a manifest read back from disk, with the raw bytes and the
// signature it was stored with.
type Cached struct {
	Manifest  Manifest
	Raw       []byte
	Signature string
}

// Save writes the manifest bytes and its signature to dir, as one file in one
// atomic rename. See cacheEnvelope for why the pair may not be split.
func Save(dir string, raw []byte, signature string) error {
	b, err := json.MarshalIndent(cacheEnvelope{Manifest: string(raw), Signature: signature}, "", "  ")
	if err != nil {
		return fmt.Errorf("relmanifest: marshal cache: %w", err)
	}
	return writeFileAtomic(ManifestPath(dir), append(b, '\n'), 0o644)
}

// Load reads the cached manifest and re-verifies it with v before returning it.
//
// Re-verifying on read is the point of storing the signature. The cache is what
// an offline box acts on (RELEASE_MANIFEST.md # Failure modes: "keeps the
// last-known manifest ... updates pause until connectivity returns"), so
// trusting it unchecked would make the local file system a way around the
// signature — anything that can write /var/lib/moose could then choose the
// version the box runs.
//
// A missing cache returns ok=false with a nil error: that is the normal state
// of a box that has never reached the CDN, not a failure.
func Load(dir string, v *Verifier) (c Cached, ok bool, err error) {
	b, err := os.ReadFile(ManifestPath(dir))
	if os.IsNotExist(err) {
		return Cached{}, false, nil
	}
	if err != nil {
		return Cached{}, false, fmt.Errorf("relmanifest: read cached manifest: %w", err)
	}
	var env cacheEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Cached{}, false, fmt.Errorf("relmanifest: parse cache %s: %w", ManifestPath(dir), err)
	}
	if env.Manifest == "" || env.Signature == "" {
		return Cached{}, false, fmt.Errorf("relmanifest: cache %s has no manifest/signature pair", ManifestPath(dir))
	}
	raw := []byte(env.Manifest)
	if _, err := v.Verify(raw, env.Signature); err != nil {
		return Cached{}, false, fmt.Errorf("relmanifest: cached manifest: %w", err)
	}
	m, err := Parse(raw)
	if err != nil {
		return Cached{}, false, err
	}
	return Cached{Manifest: m, Raw: raw, Signature: env.Signature}, true, nil
}

// writeFileAtomic writes data via a same-directory temp file, an fsync, and a
// rename, then fsyncs the directory so the rename itself survives a power cut.
// Same ceremony as the control-plane ledger, and for the same reason: this file
// is read at boot to decide what the box should run.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp beside %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename onto %s: %w", path, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s to sync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
