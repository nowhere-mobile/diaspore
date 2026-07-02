package main

import (
	"os"
	"strconv"
	"strings"
)

// On-demand media tiering (#84, P1). A roamed profile's bulk media (offline maps, ML models, downloads,
// photos) is LARGE and NOT needed to bring up a usable session, yet today it rides the login-gating CE
// restore -- so login blocks on GBs of it (profile c: 891 MB of OrganicMaps .mwm maps under
// app.organicmaps/files/). classifyDeferred decides, per file, whether it is "deferred media" -- excluded
// from the login-critical restore and served on-access from the store instead (P2/P3). This file is JUST the
// policy + the manifest shape; nothing consumes it yet (no login/seal behavior change in P1).
//
// The rule is deliberately CONSERVATIVE: a file is deferred ONLY if it is (a) big enough to matter, (b) in a
// known bulk-media subtree, and (c) NOT on the deny list of login/state-critical files. Anything unrecognized
// stays essential (restored at login), so a misclassification can only make login SLOWER, never break it.

// mediaMinBytes is the size floor for deferral (NOWHERE_MEDIA_MIN, default 8 MiB). Below this, deferring buys
// nothing (the round-trip savings don't beat just restoring it) and risks deferring small state files.
func mediaMinBytes() int64 {
	if v := os.Getenv("NOWHERE_MEDIA_MIN"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 8 << 20
}

// mediaDirs: path SEGMENTS under which large files are bulk media safe to defer. rel paths are tar-relative
// with forward slashes (e.g. "app.organicmaps/files/260527/China_Hebei.mwm", "media/0/DCIM/x.jpg").
var mediaDirs = []string{
	"/files/",    // app private files: offline maps, ML models, downloaded content
	"/download/", // user downloads
	"/dcim/",     // camera
	"/movies/", "/music/", "/pictures/", // shared media buckets
}

// mediaDenyDirs / mediaDenyExts: NEVER defer these even if large + in a media dir -- they are login- or
// state-critical (an app that can't open its DB at start crash-loops; a missing keystore breaks auth).
var mediaDenyDirs = []string{"/databases/", "/shared_prefs/", "/no_backup/"}
var mediaDenyExts = []string{".db", ".db-wal", ".db-shm", ".keystore", ".jks", ".p12", ".pem"}

// classifyDeferred reports whether a regular file (tar-relative rel, byte size) is deferred media. Pure +
// side-effect-free so P2's seal walk and P3's media daemon share ONE definition of the split.
func classifyDeferred(rel string, size int64) bool {
	if size < mediaMinBytes() {
		return false
	}
	lower := "/" + strings.ToLower(strings.TrimPrefix(rel, "/")) // leading slash so "/seg/" matches a first segment
	for _, ext := range mediaDenyExts {
		if strings.HasSuffix(lower, ext) {
			return false
		}
	}
	for _, deny := range mediaDenyDirs {
		if strings.Contains(lower, deny) {
			return false
		}
	}
	for _, dir := range mediaDirs {
		if strings.Contains(lower, dir) {
			return true
		}
	}
	return false // unrecognized large file -> essential (conservative: never break login, only slow it)
}
