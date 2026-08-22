package app

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// selectModelTTY presents an interactive scrollable model list in the
// terminal and returns the selected model ID (without any context suffix —
// the caller appends "[1m]" when appropriate).
//
// Only models that have a free variant (a "-free" suffix in the upstream
// catalog) are shown, since the default launch tier is public/free.
//
// Models are sorted by context window descending; unknown-context models
// sort to the end. Each row shows the model ID, context size, and a "[1m]"
// marker for models with ≥1M context.
//
// Navigation: ↑/↓ to move, Enter to select, Esc or Ctrl+C to cancel.
//
// If stdin is not a terminal the function returns an empty string with a
// stderr hint, so the launch flow can fall back to Claude Code's defaults.
// On cancel the return is also an empty string.
func selectModelTTY(modelIDs []string, catalog modelsDevCatalog) (string, error) {
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		fmt.Fprintln(os.Stderr, "opencode2api: stdin is not a terminal; skipping model selection")
		return "", nil
	}

	if len(modelIDs) == 0 {
		return "", nil
	}

	// Build a set of all raw model IDs for free-variant detection.
	idSet := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		idSet[id] = true
	}

	// Deduplicate to public-facing model IDs (strip "-free" suffix) and
	// keep only models that have a free variant in the caches.
	type entry struct {
		id  string
		ctx int
	}
	seen := map[string]bool{}
	var entries []entry
	for _, id := range modelIDs {
		pub := publicFacingModelID(id)
		if pub == "" || seen[pub] {
			continue
		}
		seen[pub] = true
		// Only show models with a free variant.
		if !idSet[pub+"-free"] && !isFreeModel(id) {
			continue
		}
		entries = append(entries, entry{
			id:  pub,
			ctx: getContextWindow(pub, catalog),
		})
	}
	if len(entries) == 0 {
		return "", nil
	}

	// Sort: context descending, unknown last, alphabetical for ties.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ctx != entries[j].ctx {
			return entries[i].ctx > entries[j].ctx
		}
		return entries[i].id < entries[j].id
	})

	// Put terminal in raw mode.
	rawCmd := exec.Command("stty", "raw", "-echo")
	rawCmd.Stdin = os.Stdin
	if err := rawCmd.Run(); err != nil {
		return "", err
	}
	defer func() {
		saneCmd := exec.Command("stty", "sane")
		saneCmd.Stdin = os.Stdin
		_ = saneCmd.Run()
	}()

	const pageHeight = 20
	selected := 0
	scrollTop := 0

	adjustScroll := func() {
		if selected < scrollTop {
			scrollTop = selected
		}
		if selected >= scrollTop+pageHeight {
			scrollTop = selected - pageHeight + 1
		}
		if scrollTop < 0 {
			scrollTop = 0
		}
	}

	render := func() {
		var b strings.Builder
		// Clear screen and move cursor to home.
		b.WriteString("\033[2J\033[H")
		b.WriteString("  Select a model (\xe2\x86\x91/\xe2\x86\x93 navigate, Enter select, Esc cancel)\r\n")
		b.WriteString(strings.Repeat("\xe2\x94\x80", 72))
		b.WriteString("\r\n")

		visible := pageHeight
		if visible > len(entries) {
			visible = len(entries)
		}
		for i := 0; i < visible; i++ {
			idx := scrollTop + i
			if idx >= len(entries) {
				break
			}
			e := entries[idx]
			marker := "    "
			if e.ctx >= 1000000 {
				marker = "[1m]"
			}
			ctxStr := "unknown"
			if e.ctx > 0 {
				ctxStr = fmt.Sprintf("%dK", e.ctx/1000)
			}
			prefix := "  "
			if idx == selected {
				prefix = "\xe2\x96\xb6 " // ▶
			}
			line := fmt.Sprintf("%s%-45s %10s  %s", prefix, e.id, ctxStr, marker)
			if idx == selected {
				b.WriteString("\033[36m")
				b.WriteString(line)
				b.WriteString("\033[0m")
			} else {
				b.WriteString(line)
			}
			b.WriteString("\r\n")
		}
		if len(entries) > pageHeight {
			b.WriteString("\r\n")
			b.WriteString(fmt.Sprintf("  (%d models, showing %d-%d)\r\n",
				len(entries), scrollTop+1, scrollTop+visible))
		}
		os.Stdout.WriteString(b.String())
	}

	buf := make([]byte, 8)
	for {
		render()
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return "", err
		}

		switch buf[0] {
		case 0x03: // Ctrl+C
			return "", nil
		case 0x1b: // Esc or escape sequence
			if n == 1 {
				// Bare Esc
				return "", nil
			}
			if n >= 3 && buf[1] == '[' {
				switch buf[2] {
				case 'A': // up
					if selected > 0 {
						selected--
						adjustScroll()
					}
				case 'B': // down
					if selected < len(entries)-1 {
						selected++
						adjustScroll()
					}
				}
			}
		case '\r', '\n': // Enter
			return entries[selected].id, nil
		}
	}
}
