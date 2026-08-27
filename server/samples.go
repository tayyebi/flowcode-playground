package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sample is one program from flowcode's samples/ directory, served to the
// picker with enough context that a reader knows what they're about to run.
type Sample struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// loadSamples reads samples/<dir>/<name>.fc from the upstream layout, pairing
// each program with the prose from its sibling README.md.
//
// Read once at startup: the directory is baked into the image and never changes
// under a running server, so re-reading per request would buy nothing.
func loadSamples(root string) ([]Sample, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read samples dir %s: %w", root, err)
	}

	var samples []Sample
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())

		matches, err := filepath.Glob(filepath.Join(dir, "*.fc"))
		if err != nil || len(matches) == 0 {
			continue
		}
		sort.Strings(matches)

		source, err := os.ReadFile(matches[0])
		if err != nil {
			continue
		}

		name, description := readmeSummary(filepath.Join(dir, "README.md"))
		if name == "" {
			name = titleize(entry.Name())
		}

		samples = append(samples, Sample{
			ID:          entry.Name(),
			Name:        name,
			Description: description,
			Source:      string(source),
		})
	}

	if len(samples) == 0 {
		return nil, fmt.Errorf("no samples found under %s", root)
	}

	// hello-world first — it is the one upstream calls "start here" — then
	// alphabetical, so the list is stable across rebuilds.
	sort.Slice(samples, func(i, j int) bool {
		if (samples[i].ID == "hello-world") != (samples[j].ID == "hello-world") {
			return samples[i].ID == "hello-world"
		}
		return samples[i].ID < samples[j].ID
	})

	return samples, nil
}

// readmeSummary extracts the first `# ` heading and the first body paragraph.
// Upstream READMEs all follow that shape; anything that doesn't just yields
// empty strings and falls back to the directory name.
func readmeSummary(path string) (title, description string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}

	var para []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "#") {
			// Stop at the first subheading after we have both pieces; the
			// sections that follow are module lists and run instructions.
			if title != "" {
				break
			}
			title = strings.TrimSpace(strings.TrimLeft(line, "# "))
			continue
		}
		if line == "" {
			if len(para) > 0 {
				break // end of the first paragraph
			}
			continue
		}
		if title != "" {
			para = append(para, line)
		}
	}

	// The prose wraps across lines in the source; join it back into a sentence.
	description = strings.Join(para, " ")
	// Markdown emphasis reads as noise in a <select> and a plain description.
	description = strings.NewReplacer("**", "", "`", "").Replace(description)
	return title, description
}

func titleize(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Baked into the image, so it is immutable for the life of this container.
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, s.samples)
}
