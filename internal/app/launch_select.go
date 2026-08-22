package app

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// launchModelSelectionEntry is a filtered, deduplicated, and sorted public
// model entry used by the interactive launch model selectors.
type launchModelSelectionEntry struct {
	ID            string
	ContextWindow int
}

// modelSelectionEntries converts the cached model IDs into the entries shown
// by interactive launch selectors.
//
// Only models that have a free variant (a "-free" suffix in the upstream
// catalog) are shown, since the default launch tier is public/free. Models
// are deduplicated by their public-facing ID, sorted by context window
// descending, then alphabetically.
func modelSelectionEntries(modelIDs []string, catalog modelsDevCatalog) []launchModelSelectionEntry {
	idSet := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		idSet[id] = true
	}

	seen := make(map[string]bool, len(modelIDs))
	var entries []launchModelSelectionEntry
	for _, id := range modelIDs {
		pub := publicFacingModelID(id)
		if pub == "" || seen[pub] {
			continue
		}
		seen[pub] = true
		if !idSet[pub+"-free"] && !isFreeModel(id) {
			continue
		}
		entries = append(entries, launchModelSelectionEntry{
			ID:            pub,
			ContextWindow: getContextWindow(pub, catalog),
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ContextWindow != entries[j].ContextWindow {
			return entries[i].ContextWindow > entries[j].ContextWindow
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

// selectModelNumbered implements the Windows launch selector: print a numbered
// list, read a number from in, and return the matching model ID. Empty input
// or EOF returns an empty model ID instead of crashing.
func selectModelNumbered(in io.Reader, out io.Writer, errOut io.Writer, entries []launchModelSelectionEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}

	scanner := bufio.NewScanner(in)
	for {
		fmt.Fprintln(out, "Select a model by number:")
		for i, entry := range entries {
			ctxStr := "unknown"
			if entry.ContextWindow > 0 {
				ctxStr = fmt.Sprintf("%dK", entry.ContextWindow/1000)
			}
			marker := "    "
			if entry.ContextWindow >= 1000000 {
				marker = "[1m]"
			}
			fmt.Fprintf(out, "[%d] %-45s %10s  %s\n", i+1, entry.ID, ctxStr, marker)
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", err
			}
			fmt.Fprintln(errOut, "opencode2api: no model selected; using CLI defaults")
			return "", nil
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Fprintln(errOut, "opencode2api: no model selected; using CLI defaults")
			return "", nil
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(entries) {
			fmt.Fprintf(errOut, "opencode2api: invalid selection %q; enter a number from 1 to %d\n", line, len(entries))
			continue
		}
		return entries[n-1].ID, nil
	}
}
