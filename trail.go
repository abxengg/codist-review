package review

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/abxengg/codist-review/store"
)

// A person should be able to see everything Codist decided about a commit, not only what it kept: what the model
// understood in each file, which of its claims were thrown away and why, and how each claim that was kept or
// demoted got there. The trail records this while a review runs and is stored with the commit.

// Trail is what happened in one model review of a commit.
type Trail struct {
	Files     []TrailFile            `json:"files"`     // what the model understood in each file it read
	Discarded []Discarded            `json:"discarded"` // claims the model made that Codist did not keep, with why
	Steps     map[string][]TrailStep `json:"steps"`     // by finding key: each check a kept or demoted claim went through
	mu        sync.Mutex
}

// TrailFile is the model's own reading of one file.
type TrailFile struct {
	Path     string `json:"path"`
	Reading  string `json:"reading,omitempty"` // "first", "second" or a specialist lens
	Change   string `json:"change,omitempty"`
	Effect   string `json:"effect,omitempty"`
	Expected string `json:"expected,omitempty"`
	Error    string `json:"error,omitempty"` // the model could not read it
}

// Discarded is a claim the model made that Codist did not keep.
type Discarded struct {
	Path  string `json:"path"`
	Line  int    `json:"line"`
	Title string `json:"title"`
	Why   string `json:"why"`
}

// TrailStep is one check a claim went through.
type TrailStep struct {
	Step   string `json:"step"`   // what was done: "quote", "exact check", "second look", "choice", "filter", ...
	Result string `json:"result"` // passed, kept, demoted, confirmed, refuted, ...
	Detail string `json:"detail,omitempty"`
}

func (t *Trail) file(f TrailFile) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Files = append(t.Files, f)
}

func (t *Trail) discard(path string, line int, title, why string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Discarded = append(t.Discarded, Discarded{path, line, title, why})
}

func (t *Trail) step(key, step, result, format string, args ...any) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Steps == nil {
		t.Steps = map[string][]TrailStep{}
	}
	t.Steps[key] = append(t.Steps[key], TrailStep{step, result, fmt.Sprintf(format, args...)})
}

// save stores the trail with the commit. A review that did not ask the model leaves the last trail as it was.
func (t *Trail) save(st *store.Store, sha string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	raw, err := json.Marshal(t)
	t.mu.Unlock()
	if err == nil {
		warnIf(st.SetTrail(sha, raw), "review trail not saved", "sha", sha[:min(7, len(sha))])
	}
}
