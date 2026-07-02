package main

// Incremental seal (#69, DIA-20260701-06): the legacy seal tars the WHOLE directory and CDC-chunks that one
// stream, so every save re-READS + re-HASHES the entire working set (the content cache skips re-SEALING
// unchanged chunks, but not the scan). For a multi-GB profile that's 45s warm / 2m+ cold on every logoff.
//
// This chunks FILE-ALIGNED: the CDC rolling hash resets at each large-file boundary, so a large file's chunks
// are self-contained. A per-profile scan cache (relpath -> mtime,size,mode,chunk-hashes) then lets an
// UNCHANGED large file be emitted from its cached hashes WITHOUT reading it -- the multi-GB read disappears.
// Small files (< threshold) are GROUPED and chunked normally (re-hashed each save, but their total is tiny),
// so a directory of thousands of small files doesn't explode into thousands of tiny chunks.
//
// The manifest is UNCHANGED (an ordered list of chunk hashes): restore just concatenates chunk plaintexts ->
// the same tar -> untar, regardless of how the seal split them. So restore needs no change, and correctness
// reduces to one invariant, covered by the round-trip tests: the emitted chunks concatenate to EXACTLY
// tarDirTo(src). The first save after this ships re-chunks file-aligned (hashes differ from the old
// whole-stream chunks) so it re-uploads once; after that, incremental.

import (
	"archive/tar"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// incrementalSeal is on by default; NOWHERE_SEAL_FULL=1 forces the legacy whole-tar CDC (kill switch).
func incrementalSeal() bool { return os.Getenv("NOWHERE_SEAL_FULL") != "1" }

// sealFileThreshold: files >= this are chunked file-aligned + scan-cached (an unchanged one is skipped);
// smaller files are grouped + chunked normally. Default 1 MiB.
func sealFileThreshold() int64 {
	if n, err := strconv.ParseInt(os.Getenv("NOWHERE_SEAL_FILE_THRESHOLD"), 10, 64); err == nil && n > 0 {
		return n
	}
	return 1 << 20
}

// sealFullEvery forces a full re-hash (ignore the scan cache) every Nth seal per profile, so a content change
// that somehow kept an identical mtime+size (which the cache would miss) self-corrects within N saves. The
// counter is a tiny per-tag file next to the caches. Default 20; 0 disables the periodic full re-hash.
func sealFullEvery() int {
	if n, err := strconv.Atoi(os.Getenv("NOWHERE_SEAL_FULL_EVERY")); err == nil && n >= 0 {
		return n
	}
	return 20
}

type scanEntry struct {
	Mtime  int64    `json:"m"`
	Size   int64    `json:"s"`
	Mode   uint32   `json:"o"`
	Chunks []string `json:"c"`
}
type scanRec struct {
	R string    `json:"r"`
	E scanEntry `json:"e"`
}

// fileScanCache maps a profile's large files to their sealed chunk hashes, keyed by the same DK tag as the
// content cache (so it's per-identity + per-store, and unreadable across profiles). Persisted as one JSON
// record per line under NOWHERE_BLOBCACHE.
type fileScanCache struct {
	mu  sync.Mutex
	tag string
	m   map[string]scanEntry
}

func scanCachePath(tag string) string {
	dir := os.Getenv("NOWHERE_BLOBCACHE")
	if dir == "" || tag == "" {
		return ""
	}
	return filepath.Join(dir, "scancache."+tag)
}

func newFileScanCache(dk []byte) *fileScanCache {
	sc := &fileScanCache{tag: contentCacheTagFor(dk), m: map[string]scanEntry{}}
	if p := scanCachePath(sc.tag); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if line == "" {
					continue
				}
				var rec scanRec
				if json.Unmarshal([]byte(line), &rec) == nil && rec.R != "" {
					sc.m[rec.R] = rec.E
				}
			}
		}
	}
	return sc
}

// get returns the cached chunk hashes for rel IFF (mtime,size,mode) match AND every chunk is still
// known-present in the store. A GC'd chunk (not known-present) forces a re-chunk -- reusing it would point the
// head at a missing blob. Returns nil to mean "re-chunk", which is always safe.
func (sc *fileScanCache) get(rel string, mtime, size int64, mode uint32) []string {
	sc.mu.Lock()
	e, ok := sc.m[rel]
	sc.mu.Unlock()
	if !ok || e.Mtime != mtime || e.Size != size || e.Mode != mode || len(e.Chunks) == 0 {
		return nil
	}
	for _, h := range e.Chunks {
		if !blobCacheKnown(h) {
			return nil
		}
	}
	return e.Chunks
}

func (sc *fileScanCache) put(rel string, mtime, size int64, mode uint32, chunks []string) {
	sc.mu.Lock()
	sc.m[rel] = scanEntry{Mtime: mtime, Size: size, Mode: mode, Chunks: append([]string(nil), chunks...)}
	sc.mu.Unlock()
}

// save rewrites the cache atomically, keeping only the rels seen in THIS seal so a deleted file's entry can't
// linger and pin its (now unreferenced) chunks in the cache forever. Best-effort.
func (sc *fileScanCache) save(keep map[string]bool) {
	p := scanCachePath(sc.tag)
	if p == "" {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	for rel, e := range sc.m {
		if keep != nil && !keep[rel] {
			continue
		}
		if b, err := json.Marshal(scanRec{R: rel, E: e}); err == nil {
			f.Write(b)
			f.Write([]byte("\n"))
		}
	}
	f.Close()
	os.Rename(tmp, p)
}

// sealCounterNext reads+increments a tiny per-tag seal counter and reports whether THIS seal should be a full
// re-hash (every sealFullEvery()th). Best-effort: a missing/unwritable counter just never forces a full.
func sealCounterNext(tag string) bool {
	every := sealFullEvery()
	if every <= 0 || tag == "" {
		return false
	}
	dir := os.Getenv("NOWHERE_BLOBCACHE")
	if dir == "" {
		return false
	}
	p := filepath.Join(dir, "sealcount."+tag)
	n := 0
	if b, err := os.ReadFile(p); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	os.WriteFile(p, []byte(strconv.Itoa(n)), 0o600)
	return n%every == 0
}

type scanRange struct {
	rel         string
	mtime, size int64
	mode        uint32
	start, end  int // [start,end) indices into the manifest for this freshly-chunked file
}

type walkEnt struct {
	path, rel string
	info      os.FileInfo
}

// walkChunks walks src in the SAME order as tarDirTo, driving the seal. An unchanged large file (scan-cache
// hit) reuses its stored chunk hashes via emitCached (no read); every other byte is chunked -- small files
// grouped, large files each in their own file-aligned region -- and sent to emitSeal. emitSeal/emitCached
// return the manifest index they assigned. `bump` advances the progress counter by the bytes a skipped file
// contributed (sealed bytes are counted by the caller as they finish). Returns the index ranges of freshly
// chunked large files (to refresh the scan cache after the seal lands) and the set of large-file rels seen
// (to prune deleted files). When forceFull is set, the scan cache is ignored (every large file is re-chunked).
func walkChunks(src string, sc *fileScanCache, threshold int64, forceFull bool,
	emitSeal func([]byte) int, emitCached func(string) int, bump func(int64)) (ranges []scanRange, seen map[string]bool) {
	seen = map[string]bool{}
	var group []walkEnt
	flushGroup := func() {
		if len(group) == 0 {
			return
		}
		g := group
		group = nil
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeEntriesTar(pw, g)) }()
		cdcSplit(pr, func(c []byte) { emitSeal(c) })
	}
	filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // a path vanished under the live user -> skip, matches tarDirTo
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if info.Mode().IsRegular() && info.Size() >= threshold {
			seen[rel] = true
			flushGroup() // a large file forces a chunk boundary -> its region is self-contained
			mt, sz, md := info.ModTime().UnixNano(), info.Size(), uint32(info.Mode())
			if cached := sc.getIf(!forceFull, rel, mt, sz, md); cached != nil {
				for _, h := range cached {
					emitCached(h)
				}
				bump(sz) // skipped the read -> credit its bytes to progress at once
			} else {
				start, end := -1, -1
				pth, inf := p, info
				pr, pw := io.Pipe()
				go func() { pw.CloseWithError(writeEntryTar(pw, pth, inf, rel)) }()
				cdcSplit(pr, func(c []byte) {
					i := emitSeal(c)
					if start < 0 {
						start = i
					}
					end = i
				})
				if start >= 0 {
					ranges = append(ranges, scanRange{rel, mt, sz, md, start, end + 1})
				}
			}
		} else {
			group = append(group, walkEnt{p, rel, info})
		}
		return nil
	})
	flushGroup()
	// the tar footer (two zero blocks) -- matches tarDirTo's tw.Close(); constant bytes -> one deduped chunk.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTarFooter(pw)) }()
	cdcSplit(pr, func(c []byte) { emitSeal(c) })
	return ranges, seen
}

// getIf is get() gated on `use` (false => always a miss, for a forced full re-hash).
func (sc *fileScanCache) getIf(use bool, rel string, mtime, size int64, mode uint32) []string {
	if !use {
		return nil
	}
	return sc.get(rel, mtime, size, mode)
}

// --- tar-region writers. writeTarEntry is the SINGLE source of per-entry tar bytes, shared with tarDirTo, so
// the incremental and legacy paths can never drift. Each region is Flush()ed (pads the entry) but NOT Closed
// (no footer); the footer is one final region, so the regions concatenate to exactly tarDirTo's output. ---

func writeTarEntry(tw *tar.Writer, p string, info os.FileInfo, rel string) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return nil // matches tarDirTo: a bad header skips the entry rather than aborting the seal
	}
	hdr.Name = rel
	if !info.Mode().IsRegular() {
		return tw.WriteHeader(hdr)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil // vanished after the walk -> skip entirely (no header), matches tarDirTo
	}
	defer f.Close()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.CopyN(tw, f, hdr.Size)
	if err != nil && err != io.EOF {
		return err
	}
	if n < hdr.Size { // shrank mid-seal: zero-pad to the declared size so the tar stays consistent
		_, err = io.CopyN(tw, zeroReader{}, hdr.Size-n)
		return err
	}
	return nil
}

func writeEntryTar(w io.Writer, p string, info os.FileInfo, rel string) error {
	tw := tar.NewWriter(w)
	if err := writeTarEntry(tw, p, info, rel); err != nil {
		return err
	}
	return tw.Flush()
}

func writeEntriesTar(w io.Writer, ents []walkEnt) error {
	tw := tar.NewWriter(w)
	for _, e := range ents {
		if err := writeTarEntry(tw, e.path, e.info, e.rel); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeTarFooter(w io.Writer) error {
	return tar.NewWriter(w).Close() // no entries -> just the two zero blocks
}
