package review

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/abxengg/codist-git"
	"github.com/abxengg/codist-model"
	"github.com/abxengg/codist-review/store"
)

//go:embed review.md
var systemPrompt string

// text is a string field of a reply with a limit on its length. Without limits a model can write until the reply
// is cut off, and a cut-off reply is not JSON: Qwen 3.5 did that on a third of its calls.
func text(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }

// schema forces the model to return a quote and a line for every finding.
var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		// Written before the findings (keys go out in alphabetical order): a small model judges a change better once
		// it has said what the change is, who sees a different result, and what the rules say that result must be.
		"change":   text(300),
		"effect":   text(400),
		"expected": text(500), // the rules outrank the commit message
		"findings": map[string]any{
			"type":     "array",
			"maxItems": 5,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"line":       map[string]any{"type": "integer"},
					"endLine":    map[string]any{"type": "integer"},
					"severity":   map[string]any{"type": "string", "enum": severities},
					"title":      text(120),
					"evidence":   text(300),
					"detail":     text(600),
					"suggestion": text(400),
				},
				"required": []string{"line", "severity", "title", "evidence", "detail"},
			},
		},
	},
	"required": []string{"change", "effect", "expected", "findings"},
}

type modelFinding struct {
	Line       int    `json:"line"`
	EndLine    int    `json:"endLine"`
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	Evidence   string `json:"evidence"`
	Detail     string `json:"detail"`
	Suggestion string `json:"suggestion"`
}

// maxWholeFile is the size up to which the model sees the whole file. Longer files are cut to the
// changed methods, because every 1,000 prompt tokens costs about 20 seconds on a laptop and a long
// prompt distracts a small model.
const maxWholeFile = 300

// modelReview asks the model about one file. system is the instructions plus what stays the same for every file (the
// team's rules and judged examples), so the model server can reuse its work on that opening part between calls.
func (r *Reviewer) modelReview(ctx context.Context, c git.Commit, ch git.FileChange, f Parsed, known []store.Finding, system, related string, st *Stats) ([]store.Finding, int, error) {
	var out struct {
		Change   string         `json:"change"`
		Effect   string         `json:"effect"`
		Expected string         `json:"expected"`
		Findings []modelFinding `json:"findings"`
	}
	lang := "text"
	if l := r.langOf(ch.Path); l != nil {
		lang = l.Name()
	}
	prompt := userPrompt(c, ch, f, lang, known) + related
	key := cacheKey(r.Model.Name, system+"\x00"+prompt)
	if raw, ok := r.Store.CachedReply(key); ok && json.Unmarshal(raw, &out) == nil {
		st.Cached++
		logger().Debug("model reply from cache", "sha", c.Short, "path", ch.Path)
	} else {
		u, err := r.Model.ChatJSON(model.WithLabel(ctx, model.Label{Kind: "review", Title: "Review " + path.Base(ch.Path), SHA: c.Short, Path: ch.Path}), system, prompt, schema, &out)
		if err != nil {
			return nil, 0, err
		}
		st.add(u)
		if raw, err := json.Marshal(out); err == nil {
			warnIf(r.Store.SetCachedReply(key, raw), "model reply not cached", "sha", c.Short, "path", ch.Path)
		}
	}
	logger().Debug("model read the change", "sha", c.Short, "path", ch.Path, "change", out.Change, "effect", out.Effect, "expected", out.Expected)
	r.trail.file(TrailFile{Path: ch.Path, Reading: cmp.Or(r.reading, "first"), Change: out.Change, Effect: out.Effect, Expected: out.Expected})
	var got []store.Finding
	dropped := 0
	for _, m := range out.Findings {
		drop, severity, doubt := judge(f.Lines(), ch.Patch, m.Line, strings.ToLower(m.Severity), m.Title, m.Evidence)
		if drop != "" || repeatsRule(known, ch.Path, m.Line) {
			r.trail.discard(ch.Path, m.Line, strings.TrimSpace(m.Title), cmp.Or(drop, "A code check already reports this line."))
			logger().Debug("model finding dropped", "sha", c.Short, "path", ch.Path, "line", m.Line, "title", m.Title, "why", cmp.Or(drop, "a code check already reported it"))
			dropped++
			continue
		}
		fd := finding(c.SHA, ch.Path, f, m.Line, m.EndLine, severity, "model", "", strings.TrimSpace(m.Title), m.Detail, m.Suggestion, doubt)
		// A model shown six calls that changed may report the same thing six times: once is enough.
		if slices.ContainsFunc(got, func(g store.Finding) bool { return g.Key == fd.Key }) {
			r.trail.discard(ch.Path, m.Line, fd.Title, "The model reported the same thing twice in one reply.")
			dropped++
			continue
		}
		r.trail.step(fd.Key, "quote", "passed", "The model quoted %q, and that code is at the line it named.", clipText(strings.TrimSpace(m.Evidence), 160))
		if doubt != "" {
			r.trail.step(fd.Key, "quote", "note", "%s", doubt)
		}
		got = append(got, fd)
	}
	return got, dropped, nil
}

// repeatsRule says a code check already reported this line; the check's wording is exact.
func repeatsRule(known []store.Finding, path string, line int) bool {
	for _, k := range known {
		// A note is not a report: what a run showed without saying it is wrong must not silence the model on that line.
		if k.Source == "rule" && k.Doubt == "" && k.Path == path && line >= k.Line-1 && line <= k.EndLine+1 {
			return true
		}
	}
	return false
}

func userPrompt(c git.Commit, ch git.FileChange, f Parsed, lang string, known []store.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Commit\n%s\n%s\n\n", c.Subject, c.Body)
	changed := map[int]bool{}
	for _, l := range ch.Added {
		changed[l] = true
	}
	var already []string
	for _, k := range known {
		if k.Path == ch.Path && k.Doubt == "" {
			already = append(already, fmt.Sprintf("- line %d: %s", k.Line, k.Title))
		}
	}
	if len(already) > 0 {
		fmt.Fprintf(&b, "## Already found by code checks (do not repeat)\n%s\n\n", strings.Join(already, "\n"))
	}
	patch := ch.Patch
	if len(patch) > 6000 {
		patch = patch[:6000] + "\n… (diff cut)"
	}
	fmt.Fprintf(&b, "## Diff of %s\n```diff\n%s```\n\n", ch.Path, patch)
	fmt.Fprintf(&b, "## %s after the commit (\">>\" marks changed lines)\n```%s\n", ch.Path, lang)
	lines := f.Lines()
	for _, r := range shownRanges(f, changed) {
		if r[0] > 1 {
			b.WriteString("     …\n")
		}
		for l := r[0]; l <= r[1]; l++ {
			mark := "  "
			if changed[l] {
				mark = ">>"
			}
			fmt.Fprintf(&b, "%s%4d| %s\n", mark, l, lines[l-1])
		}
	}
	b.WriteString("```\n")
	return b.String()
}

// shownRanges picks the lines the model sees: the whole file when it is short; otherwise the
// class header and fields, and each method that contains a changed line, with a little context.
func shownRanges(f Parsed, changed map[int]bool) [][2]int {
	lines := f.Lines()
	n := len(lines)
	if n <= maxWholeFile {
		return [][2]int{{1, n}}
	}
	var rs [][2]int
	for _, u := range f.Units() {
		rs = append(rs, [2]int{max(1, u.Start-3), min(n, u.HeaderEnd)})
		for _, m := range u.Members {
			for l := m[0]; l <= m[1]; l++ {
				if changed[l] {
					rs = append(rs, [2]int{max(1, docStart(lines, m[0])-1), min(n, m[1]+1)}) // with its doc comment
					break
				}
			}
		}
	}
	for l := range changed { // changed lines outside any method, such as fields
		rs = append(rs, [2]int{max(1, l-3), min(n, l+3)})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i][0] < rs[j][0] })
	var merged [][2]int
	for _, r := range rs {
		if len(merged) > 0 && r[0] <= merged[len(merged)-1][1]+1 {
			merged[len(merged)-1][1] = max(merged[len(merged)-1][1], r[1])
			continue
		}
		merged = append(merged, r)
	}
	return merged
}
