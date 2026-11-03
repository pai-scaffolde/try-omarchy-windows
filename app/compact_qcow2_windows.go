//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// compactQcow2Logged compacts disk and writes the result to the session log.
func compactQcow2Logged(disk string) error {
	logf("compact: freeing unused qcow2 clusters in %s", disk)
	before, beforeErr := platformAllocatedFileBytes(disk)
	reclaimed, err := compactQcow2(disk)
	if err != nil {
		logf("compact: %v", err)
		return err
	}
	after, afterErr := platformAllocatedFileBytes(disk)
	if beforeErr == nil && afterErr == nil {
		logf("compact: %s of unused clusters turned into holes; allocated %s -> %s", formatGiB(reclaimed), formatGiB(before), formatGiB(after))
	} else {
		logf("compact: %s of unused clusters turned into holes", formatGiB(reclaimed))
	}
	return nil
}

// runOfflineCompact handles -compact: compact a stopped standard VM's QCOW2
// disk and return the exit code. Holding the lifecycle port for the whole run
// keeps another launcher from starting the VM underneath it.
func runOfflineCompact(cfg *config) int {
	logFile, _ = os.OpenFile(filepath.Join(cfg.vmDir, "shell.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if cfg.diskFormat != "qcow2" {
		logf("compact: -compact needs a qcow2 disk, and %s is %s", cfg.disk, cfg.diskFormat)
		return 2
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", lifecyclePort))
	if err != nil {
		logf("compact: a launcher is running, so the disk is in use; stop Omarchy first")
		return 3
	}
	defer l.Close()
	if compactQcow2Logged(cfg.disk) != nil {
		return 1
	}
	return 0
}
