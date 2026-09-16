package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"
)

// unpackedDiskBytes computes the image's true on-disk size — the sum of its
// uncompressed layer sizes (APP_STORE.md # Catalog schema) — by streaming
// `docker save` and decompress-counting each layer blob. `docker image inspect
// {{.Size}}` is NOT that number on every store: under the classic overlay2
// graph driver it is the unpacked layer total, but under the containerd image
// store it reports the compressed content size, ≈ download_bytes, so resolves
// run on such a machine undercount the footprint 2–4× (issue #117). The save
// stream carries the original blobs on both stores (compressed under
// containerd, unpacked layer.tar files under the graph drivers), so counting
// decompressed bytes is store-agnostic.
func unpackedDiskBytes(ctx context.Context, image string) (int64, error) {
	cmd := exec.CommandContext(ctx, "docker", "save", image)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("docker save %s: %w", image, err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("docker save %s: %w", image, err)
	}
	n, perr := sumUnpackedLayers(stdout)
	if perr != nil {
		_, _ = io.Copy(io.Discard, stdout) // unblock docker so Wait can reap it
	}
	if werr := cmd.Wait(); werr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = werr.Error()
		}
		if perr != nil {
			return 0, fmt.Errorf("docker save %s: %s (parse error: %w)", image, msg, perr)
		}
		return 0, fmt.Errorf("docker save %s: %s", image, msg)
	}
	if perr != nil {
		return 0, fmt.Errorf("docker save %s: %w", image, perr)
	}
	return n, nil
}

// errZstd marks a blob compressed with zstd, which the resolver does not
// decompress: the stdlib has no zstd reader and the dependency isn't worth it
// until a curated image actually ships zstd layers (registries are ~all gzip
// today). Only fatal when the blob turns out to be a layer.
var errZstd = errors.New("zstd-compressed layer (the resolver handles gzip and uncompressed layers)")

// sumUnpackedLayers reads a `docker save` tar stream and returns the sum of
// the image's uncompressed layer sizes. The stream's own manifest.json names
// which entries are the layers — both save formats write it: the legacy layout
// points at <id>/layer.tar files (already uncompressed), the containerd OCI
// layout at blobs/sha256/<digest> (the original gzip blobs) — so everything
// else (config, index.json, repositories) is ignored. Exactly one image entry
// is required: a stream describing several (a multi-platform save) fails loud
// rather than over-counting.
func sumUnpackedLayers(r io.Reader) (int64, error) {
	tr := tar.NewReader(r)
	sizes := map[string]int64{}
	zstd := map[string]bool{}
	var manifestJSON []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read save stream: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(hdr.Name)
		if name == "manifest.json" {
			if manifestJSON, err = io.ReadAll(tr); err != nil {
				return 0, fmt.Errorf("read manifest.json: %w", err)
			}
			continue
		}
		n, err := decompressedCount(tr)
		if errors.Is(err, errZstd) {
			zstd[name] = true
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("entry %s: %w", name, err)
		}
		sizes[name] = n
	}
	if manifestJSON == nil {
		return 0, errors.New("save stream carries no manifest.json")
	}
	var entries []struct {
		Layers []string `json:"Layers"`
	}
	if err := json.Unmarshal(manifestJSON, &entries); err != nil {
		return 0, fmt.Errorf("parse manifest.json: %w", err)
	}
	if len(entries) != 1 {
		return 0, fmt.Errorf("save stream describes %d images, want exactly 1 (a multi-platform copy of the tag? the resolver pulls linux/amd64 only)", len(entries))
	}
	var total int64
	for _, layer := range entries[0].Layers {
		name := path.Clean(layer)
		if zstd[name] {
			return 0, fmt.Errorf("layer %s: %w", layer, errZstd)
		}
		n, ok := sizes[name]
		if !ok {
			return 0, fmt.Errorf("layer %s named by manifest.json is missing from the stream", layer)
		}
		total += n
	}
	return total, nil
}

// decompressedCount counts the bytes one blob unpacks to: through gzip when
// the magic bytes say so, the raw byte count otherwise (a legacy layer.tar is
// already uncompressed). zstd is recognized but not decompressed (errZstd).
func decompressedCount(r io.Reader) (int64, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b: // gzip
		gz, err := gzip.NewReader(br)
		if err != nil {
			return 0, err
		}
		defer gz.Close()
		return io.Copy(io.Discard, gz)
	case len(magic) == 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd: // zstd
		return 0, errZstd
	default:
		return io.Copy(io.Discard, br)
	}
}
