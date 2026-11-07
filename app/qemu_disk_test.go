package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	raw := strings.Join(driveArgs(&config{disk: disk, diskFormat: format}), " ")
	if !strings.Contains(raw, ",format=raw,if=none,id=disk0") || strings.Contains(raw, "discard") || strings.Contains(raw, "detect-zeroes") {
		t.Fatalf("raw drive: %s", raw)
	}

	writeStandaloneQcow2Header(t, filepath.Join(vm, "disk.qcow2"), 128<<20)
	disk, format = selectDisk(vm, false)
	if format != "qcow2" || disk != filepath.Join(vm, "disk.qcow2") {
		t.Fatalf("standard install with a QCOW2 disk: %s %s", disk, format)
	}
	cfg := &config{dir: dir, vmDir: vm, disk: disk, diskFormat: format}
	if arg := strings.Join(driveArgs(cfg), " "); !strings.Contains(arg, ",format=qcow2,if=none,id=disk0,discard=unmap,detect-zeroes=unmap ") {
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

func TestDiskQueuesUseIOThreads(t *testing.T) {
	for _, tc := range []struct {
		cpus, threads int
	}{{cpus: 24, threads: diskIOThreads}, {cpus: 2, threads: 2}, {cpus: 0, threads: min(minimumAutoGuestCPUs, diskIOThreads)}} {
		args := driveArgs(&config{disk: "disk.raw", diskFormat: "raw", cpus: tc.cpus})
		objects := 0
		for i, arg := range args {
			if arg == "-object" {
				objects++
				if want := fmt.Sprintf("iothread,id=disk-io%d", objects-1); args[i+1] != want {
					t.Fatalf("cpus=%d: object %q, want %q", tc.cpus, args[i+1], want)
				}
			}
		}
		if objects != tc.threads {
			t.Fatalf("cpus=%d: %d iothreads, want %d: %v", tc.cpus, objects, tc.threads, args)
		}
		var device struct {
			Driver  string `json:"driver"`
			Drive   string `json:"drive"`
			Mapping []struct {
				IOThread string `json:"iothread"`
			} `json:"iothread-vq-mapping"`
		}
		if args[len(args)-2] != "-device" {
			t.Fatalf("cpus=%d: no device: %v", tc.cpus, args)
		}
		if err := json.Unmarshal([]byte(args[len(args)-1]), &device); err != nil {
			t.Fatal(err)
		}
		if device.Driver != "virtio-blk-pci" || device.Drive != "disk0" || len(device.Mapping) != tc.threads {
			t.Fatalf("cpus=%d: device %+v", tc.cpus, device)
		}
	}
}

func TestMissingInstalledDiskRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	vm := filepath.Join(dir, "vm")
	if err := os.MkdirAll(vm, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config{dir: dir, vmDir: vm}
	cfg.disk, cfg.diskFormat = selectDisk(vm, false)
	if err := refuseMissingInstalledDisk(cfg); err != nil {
		t.Fatalf("a new installation must be able to create its disk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vm, desktopReachedName), []byte("id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := refuseMissingInstalledDisk(cfg); err == nil || !strings.Contains(err.Error(), "disk.raw") {
		t.Fatalf("missing disk of a used installation: %v", err)
	}
	fresh := *cfg
	fresh.fresh = true
	if err := refuseMissingInstalledDisk(&fresh); err != nil {
		t.Fatalf("Start fresh must replace a missing disk: %v", err)
	}
	writeStandaloneQcow2Header(t, filepath.Join(vm, "disk.qcow2"), 128<<20)
	cfg.disk, cfg.diskFormat = selectDisk(vm, false)
	if err := refuseMissingInstalledDisk(cfg); err != nil {
		t.Fatalf("present disk: %v", err)
	}
}

func TestPreviousQemuLogsAreKept(t *testing.T) {
	vm := t.TempDir()
	if err := os.WriteFile(filepath.Join(vm, "serial.log"), []byte("kernel panic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vm, "qemu-stderr.log"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := range 25 {
		if err := keepPreviousQemuLogs(vm, start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	kept, _ := filepath.Glob(filepath.Join(vm, "previous-logs", "*"))
	if len(kept) != 20 {
		t.Fatalf("kept %d logs, want 20: %v", len(kept), kept)
	}
	newest := filepath.Join(vm, "previous-logs", "20260102-030429.000-serial.log")
	if data, err := os.ReadFile(newest); err != nil || string(data) != "kernel panic\n" {
		t.Fatalf("newest copy: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(vm, "previous-logs", "20260102-030405.000-serial.log")); !os.IsNotExist(err) {
		t.Fatalf("oldest copy was not pruned: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(vm, "serial.log")); string(data) != "kernel panic\n" {
		t.Fatal("the live log must be copied, not moved")
	}
}
