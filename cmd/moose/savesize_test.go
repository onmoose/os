package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"github.com/onmoose/moose/internal/manifest"
)

// --- sumUnpackedLayers: pure save-stream parsing, no docker ----------------

type tarEntry struct {
	name string
	data []byte
}

func buildSaveStream(t *testing.T, entries []tarEntry) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func gzipped(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The containerd image store saves the original registry blobs — gzip layers
// under blobs/sha256/. The sum must be the decompressed sizes, not the blob
// sizes on the wire (the exact confusion behind #117).
func TestSumUnpackedLayers_ContainerdGzipBlobs(t *testing.T) {
	layerA := bytes.Repeat([]byte("a"), 10_000) // compresses far below 10 000
	layerB := bytes.Repeat([]byte("b"), 2_345)
	stream := buildSaveStream(t, []tarEntry{
		{"blobs/sha256/aaa", gzipped(t, layerA)},
		{"blobs/sha256/bbb", gzipped(t, layerB)},
		{"blobs/sha256/cfg", []byte(`{"rootfs":{}}`)}, // config blob: raw, not a layer
		{"index.json", []byte(`{}`)},
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"manifest.json", []byte(`[{"Config":"blobs/sha256/cfg","Layers":["blobs/sha256/aaa","blobs/sha256/bbb"]}]`)},
	})
	got, err := sumUnpackedLayers(stream)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(12_345); got != want {
		t.Fatalf("got %d, want %d (decompressed layer sum)", got, want)
	}
}

// The classic graph-driver save carries uncompressed <id>/layer.tar files;
// their raw size is the answer. manifest.json placed first to show the parser
// doesn't depend on stream order.
func TestSumUnpackedLayers_ClassicLayerTar(t *testing.T) {
	layer := bytes.Repeat([]byte("x"), 4_096)
	stream := buildSaveStream(t, []tarEntry{
		{"manifest.json", []byte(`[{"Config":"cfg.json","Layers":["abc123/layer.tar"]}]`)},
		{"cfg.json", []byte(`{}`)},
		{"abc123/layer.tar", layer},
		{"abc123/VERSION", []byte("1.0")},
	})
	got, err := sumUnpackedLayers(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got != 4_096 {
		t.Fatalf("got %d, want 4096 (raw uncompressed layer size)", got)
	}
}

func TestSumUnpackedLayers_Errors(t *testing.T) {
	zstdBlob := append([]byte{0x28, 0xb5, 0x2f, 0xfd}, bytes.Repeat([]byte("z"), 64)...)
	cases := []struct {
		name    string
		entries []tarEntry
		wantErr string
	}{
		{
			"no manifest.json",
			[]tarEntry{{"blobs/sha256/aaa", gzipped(t, []byte("data"))}},
			"no manifest.json",
		},
		{
			"layer missing from stream",
			[]tarEntry{{"manifest.json", []byte(`[{"Layers":["blobs/sha256/gone"]}]`)}},
			"missing from the stream",
		},
		{
			"multiple image entries",
			[]tarEntry{{"manifest.json", []byte(`[{"Layers":[]},{"Layers":[]}]`)}},
			"want exactly 1",
		},
		{
			"zstd layer",
			[]tarEntry{
				{"blobs/sha256/zzz", zstdBlob},
				{"manifest.json", []byte(`[{"Layers":["blobs/sha256/zzz"]}]`)},
			},
			"zstd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sumUnpackedLayers(buildSaveStream(t, tc.entries))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A zstd blob that is NOT a layer (some future non-layer artifact) must not
// fail the sum — only layers need decompressing.
func TestSumUnpackedLayers_ZstdNonLayerIgnored(t *testing.T) {
	stream := buildSaveStream(t, []tarEntry{
		{"blobs/sha256/aaa", gzipped(t, bytes.Repeat([]byte("a"), 100))},
		{"blobs/sha256/zzz", append([]byte{0x28, 0xb5, 0x2f, 0xfd}, []byte("not a layer")...)},
		{"manifest.json", []byte(`[{"Layers":["blobs/sha256/aaa"]}]`)},
	})
	got, err := sumUnpackedLayers(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got != 100 {
		t.Fatalf("got %d, want 100", got)
	}
}

func TestLowExpansion(t *testing.T) {
	cases := []struct {
		name string
		ref  manifest.ImageRef
		want bool
	}{
		{"compressed-size signature ≈1×", manifest.ImageRef{DownloadBytes: 300_086_308, DiskBytes: 300_120_809}, true},
		{"normal 3× expansion", manifest.ImageRef{DownloadBytes: 300_086_308, DiskBytes: 887_843_328}, false},
		{"exactly 1.2× is not low", manifest.ImageRef{DownloadBytes: 100, DiskBytes: 120}, false},
		{"just under 1.2×", manifest.ImageRef{DownloadBytes: 100, DiskBytes: 119}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lowExpansion(tc.ref); got != tc.want {
				t.Fatalf("lowExpansion(%+v) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}
