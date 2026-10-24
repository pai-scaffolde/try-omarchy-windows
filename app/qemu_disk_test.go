package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStandaloneQcow2Header writes the header of a QCOW2 v3 image with no
// backing file, which is all inspectInstallationDisk reads.
func writeStandaloneQcow2Header(t *testing.T, path string, virtualBytes uint64) {
	t.Helper()
	header := make([]byte, qcow2HeaderSize)
	binary.BigEndian.PutUint32(header[0:4], 0x514649fb)
	binary.BigEndian.PutUint32(header[4:8], 3)
	binary.BigEndian.PutUint32(header[20:24], 16)
	binary.BigEndian.PutUint64(header[24:32], virtualBytes)
	binary.BigEndian.PutUint32(header[100:104], qcow2HeaderSize)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, header, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStandardInstallBootsQcow2WithDiscard(t *testing.T) {
	dir := t.TempDir()
	vm := filepath.Join(dir, "vm")

	disk, format := selectDisk(vm, false)
	if format != "raw" || disk != filepath.Join(vm, "disk.raw") {
		t.Fatalf("standard install without a QCOW2 disk: %s %s", disk, format)
	}
	raw := driveArg(&config{disk: disk, diskFormat: format})
	if !strings.Contains(raw, ",format=raw,if=virtio") || strings.Contains(raw, "discard") || strings.Contains(raw, "detect-zeroes") {
		t.Fatalf("raw drive: %s", raw)
	}

	writeStandaloneQcow2Header(t, filepath.Join(vm, "disk.qcow2"), 128<<20)
	disk, format = selectDisk(vm, false)
	if format != "qcow2" || disk != filepath.Join(vm, "disk.qcow2") {
		t.Fatalf("standard install with a QCOW2 disk: %s %s", disk, format)
	}
	cfg := &config{dir: dir, vmDir: vm, disk: disk, diskFormat: format}
	if arg := driveArg(cfg); !strings.HasSuffix(arg, ",format=qcow2,if=virtio,discard=unmap,detect-zeroes=unmap") {
		t.Fatalf("qcow2 drive: %s", arg)
	}
	if _, portableFormat := selectDisk(t.TempDir(), true); portableFormat != "qcow2" {
		t.Fatalf("portable install: %s", portableFormat)
	}

	// The raw path would truncate the file up to its virtual size.
	if err := prepareDisk(cfg, 64); err != nil {
		t.Fatalf("prepare qcow2: %v", err)
	}
	if info, err := os.Stat(disk); err != nil || info.Size() != qcow2HeaderSize {
		t.Fatalf("prepare changed the qcow2 file: %v %v", info, err)
	}
	if _, err := resetStandardDisk(cfg, 64); err == nil || !strings.Contains(err.Error(), "disk.qcow2") {
		t.Fatalf("reset of a qcow2 install: %v", err)
	}
}
