package proxy

import (
	"path"
	"sort"
	"strings"

	"github.com/thefcan/turnike/internal/config"
)

// Entry is one compiled route. Prefix is the route's prefix normalized for
// matching: trailing slashes stripped, so the root prefix "/" becomes "".
type Entry struct {
	Prefix string
	Route  config.Route
}

// Table matches request paths to routes using segment-boundary
// longest-prefix matching (see config.Route for the semantics).
type Table struct {
	entries []Entry // sorted by len(Prefix) descending
}

// NewTable compiles routes into a match table. Routes are assumed to be
// config-validated (non-empty, unique up to trailing slashes).
func NewTable(routes []config.Route) *Table {
	entries := make([]Entry, len(routes))
	for i, r := range routes {
		entries[i] = Entry{Prefix: strings.TrimRight(r.Prefix, "/"), Route: r}
	}
	// Longest prefix first, so a linear scan returns the most specific match.
	sort.SliceStable(entries, func(i, j int) bool {
		return len(entries[i].Prefix) > len(entries[j].Prefix)
	})
	return &Table{entries: entries}
}

// Match returns the route for escapedPath (as from http Request
// URL.EscapedPath). The path is cleaned before matching so dot segments
// cannot sidestep a prefix, and matching runs on the escaped form so an
// encoded slash never counts as a segment boundary. A prefix matches only
// at segment boundaries: it must equal the path or be followed by "/".
func (t *Table) Match(escapedPath string) (Entry, bool) {
	p := path.Clean(escapedPath)
	for _, e := range t.entries {
		// The root prefix normalizes to "" and matches every path via the
		// HasPrefix branch, since every cleaned path starts with "/".
		if p == e.Prefix || strings.HasPrefix(p, e.Prefix+"/") {
			return e, true
		}
	}
	return Entry{}, false
}
