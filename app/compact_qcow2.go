package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// QCOW2 disks need no zero-fill. The guest's discards (fstrim) drop the
// refcount of the clusters they free to zero and QEMU reuses them, but QEMU's
// Windows file backend cannot pass the discard on, so disk.qcow2 never gets
// smaller. Once the guest is off, every cluster with a zero refcount is dead
// weight in the file and can become a hole.

const qcow2RefcountOrder = 4 // 16-bit refcounts, the qemu-img default

// qcow2FreeRuns returns the [offset, length) runs of clusters below size whose
// refcount is zero. It reads only the header, the refcount table and the
// refcount blocks, and refuses any image whose layout it does not handle
// exactly, so it never guesses which bytes are unused.
func qcow2FreeRuns(f io.ReaderAt, size int64) ([][2]int64, error) {
	h := make([]byte, qcow2HeaderSize)
	if _, err := f.ReadAt(h, 0); err != nil {
		return nil, fmt.Errorf("reading the qcow2 header: %w", err)
	}
	if string(h[0:4]) != "QFI\xfb" {
		return nil, errors.New("not a qcow2 image")
	}
	if v := binary.BigEndian.Uint32(h[4:8]); v != 3 {
		return nil, fmt.Errorf("qcow2 version %d is not supported", v)
	}
	if bits := binary.BigEndian.Uint32(h[20:24]); bits != qcow2ClusterBits {
		return nil, fmt.Errorf("qcow2 cluster size 2^%d is not supported", bits)
	}
	if order := binary.BigEndian.Uint32(h[96:100]); order != qcow2RefcountOrder {
		return nil, fmt.Errorf("qcow2 refcount order %d is not supported", order)
	}
	// Dirty, corrupt, external data file and extended L2 all change what a
	// zero refcount means, so any incompatible feature bit is a refusal.
	if features := binary.BigEndian.Uint64(h[72:80]); features != 0 {
		return nil, fmt.Errorf("qcow2 incompatible features %#x are set; run qemu-img check first", features)
	}
	tableOffset := int64(binary.BigEndian.Uint64(h[48:56]))
	tableClusters := int64(binary.BigEndian.Uint32(h[56:60]))
	if tableOffset <= 0 || tableOffset%qcow2ClusterSize != 0 || tableClusters <= 0 || tableOffset+tableClusters*qcow2ClusterSize > size {
		return nil, errors.New("qcow2 refcount table lies outside the file")
	}
	table := make([]byte, tableClusters*qcow2ClusterSize)
	if _, err := f.ReadAt(table, tableOffset); err != nil {
		return nil, fmt.Errorf("reading the qcow2 refcount table: %w", err)
	}

	clusters := (size + qcow2ClusterSize - 1) / qcow2ClusterSize
	perBlock := qcow2ClusterSize * 8 / (1 << qcow2RefcountOrder)
	if (clusters+perBlock-1)/perBlock > int64(len(table)/8) {
		return nil, errors.New("qcow2 refcount table does not cover the whole file")
	}
	var runs [][2]int64
	free := func(first, last int64) { // clusters [first, last)
		start, end := first*qcow2ClusterSize, min(last*qcow2ClusterSize, size)
		if n := len(runs); n > 0 && runs[n-1][0]+runs[n-1][1] == start {
			runs[n-1][1] += end - start
		} else {
			runs = append(runs, [2]int64{start, end - start})
		}
	}
	block := make([]byte, qcow2ClusterSize)
	for first := int64(0); first < clusters; first += perBlock {
		last := min(first+perBlock, clusters)
		entry := binary.BigEndian.Uint64(table[8*(first/perBlock):])
		if entry&0x1ff != 0 {
			return nil, errors.New("qcow2 refcount table entry has reserved bits set")
		}
		offset := int64(entry)
		if offset == 0 {
			// No refcount block: nothing in this range is in use. Cluster 0
			// is the header, which always has a block, so it never lands here.
			free(first, last)
			continue
		}
		if offset%qcow2ClusterSize != 0 || offset+qcow2ClusterSize > size {
			return nil, errors.New("qcow2 refcount block lies outside the file")
		}
		if _, err := f.ReadAt(block, offset); err != nil {
			return nil, fmt.Errorf("reading a qcow2 refcount block: %w", err)
		}
		for c := first; c < last; c++ {
			if c > 0 && binary.BigEndian.Uint16(block[2*(c-first):]) == 0 {
				free(c, c+1)
			}
		}
	}
	// Clusters the image must be using: the header, the refcount table, every
	// refcount block and the L1 table. If any of them reads as free, the
	// refcounts and the metadata disagree, so the image is damaged in a way
	// the feature bits do not show. Refuse rather than punch.
	l1Offset := int64(binary.BigEndian.Uint64(h[40:48]))
	l1Bytes := int64(binary.BigEndian.Uint32(h[36:40])) * 8
	inUse := [][2]int64{{0, qcow2ClusterSize}, {tableOffset, tableClusters * qcow2ClusterSize}}
	if l1Bytes > 0 {
		inUse = append(inUse, [2]int64{l1Offset, l1Bytes})
	}
	for i := 0; i+8 <= len(table); i += 8 {
		if offset := int64(binary.BigEndian.Uint64(table[i:])); offset != 0 {
			inUse = append(inUse, [2]int64{offset, qcow2ClusterSize})
		}
	}
	for _, m := range inUse {
		for _, r := range runs {
			if m[0] < r[0]+r[1] && r[0] < m[0]+m[1] {
				return nil, fmt.Errorf("qcow2 metadata at %d reads as unused; the refcounts are damaged, run qemu-img check", m[0])
			}
		}
	}
	return runs, nil
}

// compactQcow2 turns every unused cluster of a stopped QCOW2 disk into a hole
// and returns the bytes handed back. openDiskExclusive fails while QEMU or
// anything else still has the file open.
func compactQcow2(path string) (int64, error) {
	f, err := openDiskExclusive(path)
	if err != nil {
		return 0, fmt.Errorf("opening the disk for compaction (is Omarchy still running?): %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	runs, err := qcow2FreeRuns(f, info.Size())
	if err != nil {
		return 0, err
	}
	if err := setSparse(f); err != nil {
		return 0, fmt.Errorf("marking the disk sparse: %w", err)
	}
	var reclaimed int64
	for _, r := range runs {
		if err := punchHole(f, r[0], r[1]); err != nil {
			return reclaimed, err
		}
		reclaimed += r[1]
	}
	return reclaimed, nil
}
