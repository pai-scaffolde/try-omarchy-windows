package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// syntheticQcow2 builds a minimal v3 image: header in cluster 0, a one-cluster
// refcount table in cluster 1, one refcount block in cluster 2, then data
// clusters whose refcounts come from used (true = refcount 1).
func syntheticQcow2(used []bool) []byte {
	clusters := 3 + len(used)
	img := make([]byte, int64(clusters)*qcow2ClusterSize)
	copy(img, "QFI\xfb")
	binary.BigEndian.PutUint32(img[4:], 3)
	binary.BigEndian.PutUint32(img[20:], qcow2ClusterBits)
	binary.BigEndian.PutUint64(img[48:], uint64(qcow2ClusterSize))
	binary.BigEndian.PutUint32(img[56:], 1)
	binary.BigEndian.PutUint32(img[96:], qcow2RefcountOrder)
	binary.BigEndian.PutUint64(img[qcow2ClusterSize:], uint64(2*qcow2ClusterSize))
	block := img[2*qcow2ClusterSize:]
	for c := 0; c < 3; c++ {
		binary.BigEndian.PutUint16(block[2*c:], 1)
	}
	for i, u := range used {
		if u {
			binary.BigEndian.PutUint16(block[2*(3+i):], 1)
		}
	}
	return img
}

func TestQcow2FreeRunsCoalescesUnusedClusters(t *testing.T) {
	img := syntheticQcow2([]bool{true, false, false, true, false})
	runs, err := qcow2FreeRuns(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	cs := qcow2ClusterSize
	want := [][2]int64{{4 * cs, 2 * cs}, {7 * cs, cs}}
	if len(runs) != len(want) || runs[0] != want[0] || runs[1] != want[1] {
		t.Fatalf("runs %v, want %v", runs, want)
	}
}

func TestQcow2FreeRunsNeverReturnsMetadata(t *testing.T) {
	img := syntheticQcow2([]bool{true, true})
	runs, err := qcow2FreeRuns(bytes.NewReader(img), int64(len(img)))
	if err != nil || len(runs) != 0 {
		t.Fatalf("fully used image: runs %v err %v", runs, err)
	}
}

func TestQcow2FreeRunsTreatsMissingRefcountBlockAsFree(t *testing.T) {
	perBlock := qcow2ClusterSize * 8 / (1 << qcow2RefcountOrder)
	img := syntheticQcow2([]bool{true})
	// Grow the file past the first refcount block's range; table entry 1 is 0.
	size := (perBlock + 2) * qcow2ClusterSize
	runs, err := qcow2FreeRuns(readerWithSize{bytes.NewReader(img), size}, size)
	if err != nil {
		t.Fatal(err)
	}
	last := runs[len(runs)-1]
	if last[0]+last[1] != size || last[0] > perBlock*qcow2ClusterSize {
		t.Fatalf("last run %v does not reach the end %d", last, size)
	}
}

// readerWithSize reads zeros past the end of the wrapped image.
type readerWithSize struct {
	r    *bytes.Reader
	size int64
}

func (r readerWithSize) ReadAt(p []byte, off int64) (int, error) {
	n, _ := r.r.ReadAt(p, off)
	for i := n; i < len(p); i++ {
		p[i] = 0
	}
	return len(p), nil
}

func TestQcow2FreeRunsRefusesImagesItCannotReadExactly(t *testing.T) {
	cases := map[string]func([]byte){
		"magic":         func(b []byte) { b[0] = 'X' },
		"version 2":     func(b []byte) { binary.BigEndian.PutUint32(b[4:], 2) },
		"cluster bits":  func(b []byte) { binary.BigEndian.PutUint32(b[20:], 20) },
		"refcount bits": func(b []byte) { binary.BigEndian.PutUint32(b[96:], 5) },
		"dirty":         func(b []byte) { binary.BigEndian.PutUint64(b[72:], 1) },
		"table outside": func(b []byte) { binary.BigEndian.PutUint64(b[48:], uint64(1<<40)) },
		"block outside": func(b []byte) { binary.BigEndian.PutUint64(b[qcow2ClusterSize:], uint64(1<<40)) },
		"reserved bits": func(b []byte) { binary.BigEndian.PutUint64(b[qcow2ClusterSize:], uint64(2*qcow2ClusterSize+1)) },
	}
	for name, corrupt := range cases {
		img := syntheticQcow2([]bool{false})
		corrupt(img)
		if runs, err := qcow2FreeRuns(bytes.NewReader(img), int64(len(img))); err == nil {
			t.Errorf("%s: accepted, runs %v", name, runs)
		}
	}
}

// TestQcow2FreeRunsOnRealImage runs against an image qemu made, named by
// QCOW2_FREE_RUNS_IMAGE. It overwrites every cluster it calls free with
// garbage; qemu-img check and a pattern read afterwards prove those clusters
// were really unused.
func TestQcow2FreeRunsOnRealImage(t *testing.T) {
	path := os.Getenv("QCOW2_FREE_RUNS_IMAGE")
	if path == "" {
		t.Skip("QCOW2_FREE_RUNS_IMAGE not set")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	runs, err := qcow2FreeRuns(f, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	garbage := []byte(strings.Repeat("\xee", int(qcow2ClusterSize)))
	var total int64
	for _, r := range runs {
		for off := r[0]; off < r[0]+r[1]; off += qcow2ClusterSize {
			if _, err := f.WriteAt(garbage[:min(qcow2ClusterSize, r[0]+r[1]-off)], off); err != nil {
				t.Fatal(err)
			}
		}
		total += r[1]
	}
	t.Logf("overwrote %d bytes in %d free runs of a %d-byte image", total, len(runs), info.Size())
}
