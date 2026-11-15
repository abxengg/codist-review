// Package review reviews a commit: code checks on every changed line, then the model on the
// changed files, with each model claim checked against the code before it is shown.
package review

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abxengg/codist-git"
	"github.com/abxengg/codist-model"
	"github.com/abxengg/codist-review/store"
)

// Reviewer reviews commits of one repository.
type Reviewer struct {
	Repo  *git.Repo
	Store *store.Store
	Model *model.Client // nil means rules only
	// MaxFiles is how many changed files of a commit the model reads without planning. Above it the model first
	// decides which files need reading (see plan); every file is still considered and the decision is stored.
	MaxFiles int
	Learn    bool // show the model findings this team marked wrong or useful, as examples
	// Contracts shows the model the signature and javadoc of the methods the changed code calls in other files.
	Contracts bool
	Rules     string // the team's own rules for the model, from .codist/rules.md

	Ignored func(path string) bool
	// Languages are the languages reviewed; empty means Java only. See Language.
	Languages []Language
	head      string      // HEAD while one review runs, so it is asked of git once and not for every file
	docs      []paragraph // the project's documents at the commit under review, read when a check first needs them
	reading   string      // which reading of a file the model is on, for the trail
	trail     *Trail      // what happened in the model review under way; nil when the model is not asked
	// NoSecondLook turns off the model's second look at each serious finding it made (see secondLook). It is on
	// unless turned off: on the planted-bug test it turned false alarms on clean commits into notes.
	NoSecondLook bool
}

// Options says how much of a review to do.
type Options struct {
	RulesOnly bool // skip the model, for catching up on older commits
	// Twice has the model read each file a second time and adds what only the second reading reports. Every finding
	// still passes the same checks. For commits likely to bring a bug: it costs one more model call per file.
	Twice bool
	// Deep has the model read each file once more through each of the Lenses, a specialist's question at a time,
	// and adds what only those readings report. It costs a model call per file and lens, so it is for when the
	// machine has nothing else to read. The first reading comes from the cache when the commit was read before.
	Deep bool
	// Force lists files a person asked the model to read although the plan skipped them.
	Force []string
	Why   string // why the model was skipped, shown with the review
}

// ErrModelFailed is returned by Review when the model was asked about a commit's files and every call failed, as
// when its server stops. The code checks are stored and the review is saved without a model, so the caller can
// wait for the server and review the commit again.
var ErrModelFailed = errors.New("the model could not read this commit")

// Review reviews one commit and stores the result. Code check findings are saved first, so they
// show while the model is still working.
func (r *Reviewer) Review(ctx context.Context, sha string, opt Options) error {
	c, changes, err := r.Repo.Show(sha) // the commit and its changes in one git call
	if err != nil {
		return err
	}
	sha = c.SHA // always store under the full id, whatever form it was given in
	r.head, r.docs = r.Repo.Head(), nil
	defer func() { r.head, r.docs = "", nil }()
	started := time.Now()
	modelName := ""
	if r.Model != nil && !opt.RulesOnly {
		modelName = r.Model.Name
	}
	warnIf(r.Store.SetReview(store.Review{SHA: sha, Status: "running", Model: modelName, Started: started}), "review status not saved", "sha", c.Short)
	fail := func(err error) error {
		logger().Warn("review failed", "sha", c.Short, "took", time.Since(started).Round(time.Millisecond), "err", err)
		warnIf(r.Store.SetReview(store.Review{SHA: sha, Status: "failed", Model: modelName, Note: err.Error(), Started: started, Finished: time.Now()}),
			"review status not saved", "sha", c.Short)
		return err
	}
	for i := range changes {
		changes[i].Added = withDeletions(changes[i].Added, changes[i].Patch)
	}
	type target struct {
		change git.FileChange
		file   Parsed
	}
	var targets []target
	var findings []store.Finding
	for _, c := range changes {
		lang := r.langOf(c.Path)
		if c.Status == "deleted" || lang == nil || (r.Ignored != nil && r.Ignored(c.Path)) {
			continue
		}
		src, ok := r.Repo.FileAt(sha, c.Path)
		if !ok {
			logger().Warn("changed file not readable", "sha", sha[:7], "path", c.Path)
			continue
		}
		f := lang.Parse(c.Path, src)
		changed := map[int]bool{}
		for _, l := range c.Added {
			changed[l] = true
		}
		for _, is := range lang.Check(f, changed, r.facts(lang)) {
			findings = append(findings, finding(sha, c.Path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, is.Detail, is.Suggestion, ""))
		}
		if !lang.IsTest(c.Path) {
			findings = append(findings, r.specNumbers(sha, c, f)...)
			findings = append(findings, r.learnedFindings(sha, c.Path, f, changed)...)
		}
		targets = append(targets, target{c, f})
	}
	findings = append(findings, r.ranFindings(c, changes, func(ch git.FileChange) Parsed {
		for _, t := range targets {
			if t.change.Path == ch.Path {
				return t.file
			}
		}
		return nil
	})...)
	stats := Stats{PromptVersion: PromptVersion, Model: modelName, Deep: opt.Deep}
	logger().Debug("checks done", "sha", c.Short, "changed", len(changes), "reviewed", len(targets), "rule_findings", len(findings))
	findings = r.hideWrong(findings, &stats)
	if err := r.Store.ReplaceFindings(sha, findings); err != nil {
		return fail(err)
	}

	var notes []string
	if opt.Why != "" {
		notes = append(notes, opt.Why)
	}
	if r.Model != nil && !opt.RulesOnly {
		r.trail = &Trail{Files: []TrailFile{}, Discarded: []Discarded{}, Steps: map[string][]TrailStep{}}
		defer func() { r.trail.save(r.Store, sha); r.trail = nil }()
		commit := c
		var related map[string]string
		learned := ""
		if r.Learn {
			learned = r.feedbackExamples()
		}
		related = map[string]string{}
		if r.Contracts {
			var changed []git.FileChange
			for _, t := range targets {
				changed = append(changed, t.change)
			}
			related = r.contractContext(r.codeAt(sha), changed)
		}
		system := systemPrompt + rulesSection(r.Rules) + learned // the same for every file, so it goes first
		var triage *Triage
		if r.Learn {
			triage = r.triageFromFeedback()
		}
		// Every file is read first and every second look comes after: the model server keeps its work on the opening
		// part of a prompt only while the next prompt starts the same, and the two kinds of call start differently.
		type read struct {
			t      target
			others []git.FileChange
			got    []store.Finding
		}
		var reads []read
		shown := r.ranAt(sha) // what running the code showed, when it has been run
		var whole *code
		codeOnce := func() *code { // the code at the commit with its call graph, read the first time it is needed
			if whole == nil {
				whole = r.codeAt(sha)
			}
			return whole
		}
		var files []planned
		for _, t := range targets {
			files = append(files, planned{t.change, t.file})
		}
		plan := r.plan(ctx, commit, files, opt.Force, &stats)
		warnIf(r.Store.SetPlan(sha, plan), "reading plan not saved", "sha", c.Short)
		reviewed, failures := 0, 0
		for i, t := range targets {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			if plan[i].Decision != "read" {
				continue
			}
			reviewed++
			fileStart := time.Now()
			others := otherChanges(changes, t.change.Path)
			extra := related[t.change.Path] + othersSection(others) + r.ranSection(shown, t.change.Path)
			r.reading = "first"
			got, dropped, err := r.modelReview(ctx, commit, t.change, t.file, findings, system, extra, &stats)
			if err != nil {
				r.trail.file(TrailFile{Path: t.change.Path, Reading: "first", Error: err.Error()})
				logger().Warn("model could not review file", "sha", c.Short, "path", t.change.Path, "err", err)
				notes = append(notes, "The model could not review "+t.change.Path+": "+err.Error())
				failures++
				continue
			}
			var readings []string
			// Methods other files call whose promise got weaker: a model fact per method, joined to the callers. Only in
			// the deep pass: on the three labs it added up to 70% more model calls and none of its findings hit a bug.
			if opt.Deep {
				got = append(got, r.promiseChanges(ctx, commit, t.change, t.file, codeOnce, &stats)...)
			}
			if opt.Twice {
				readings = append(readings, secondReading)
			}
			if opt.Deep {
				readings = append(readings, Lenses...)
			}
			for _, reading := range readings {
				r.reading = readingName(reading)
				if again, _, err := r.modelReview(ctx, commit, t.change, t.file, findings, system, extra+reading, &stats); err == nil {
					got = addNew(got, again)
				}
			}
			logger().Debug("model reviewed file", "sha", c.Short, "path", t.change.Path, "kept", len(got), "dropped", dropped,
				"took", time.Since(fileStart).Round(time.Millisecond))
			if dropped > 0 {
				logger().Info("dropped model findings", "sha", c.Short, "path", t.change.Path, "count", dropped)
			}
			// exact facts first; a contradicted claim skips the second look
			reads = append(reads, read{t, others, r.contradict(sha, t.change, t.file, others, got)})
		}
		for _, rd := range reads {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			got := rd.got
			if !r.NoSecondLook {
				got = r.secondLook(ctx, commit, rd.t.change, rd.t.file.Lines(), got, &stats)
			}
			for i := range got {
				was := got[i].Doubt
				got[i] = triage.demote(got[i])
				if got[i].Doubt != was {
					r.trail.step(got[i].Key, "learned filter", "demoted", "%s", got[i].Doubt)
				}
				// What the model itself rates minor gets no second look, so it is not shown as checked: on the test
				// projects no real bug was rated this low, and the false alarms were.
				if got[i].Doubt == "" && (got[i].Severity == "low" || got[i].Severity == "info") {
					got[i].Doubt = "The model rated this minor, so Codist did not check it further."
					r.trail.step(got[i].Key, "severity", "note", "The model rated it %s.", got[i].Severity)
				}
				if got[i].Doubt == "" {
					r.trail.step(got[i].Key, "result", "kept", "Shown as an issue.")
				}
			}
			findings = append(findings, r.hideWrong(got, &stats)...)
			warnIf(r.Store.ReplaceFindings(sha, findings), "progress not saved", "sha", c.Short) // show progress file by file
		}
		if reviewed > 0 && failures == reviewed { // the model read nothing: this is not a model review
			r.resolve(sha, changes)
			r.settle(sha, findings)
			warnIf(r.Store.SetReview(store.Review{SHA: sha, Status: "done", Started: started, Finished: time.Now(),
				Note: "The model could not read this commit (" + notes[len(notes)-1] + "); the code checks are done."}), "review status not saved", "sha", c.Short)
			return ErrModelFailed
		}
	}
	// A run that finished while the model was reading added its findings to the store; saving the review's own list
	// took them out again. Put back what the run shows now.
	late := r.ranFindings(c, changes, func(ch git.FileChange) Parsed {
		for _, t := range targets {
			if t.change.Path == ch.Path {
				return t.file
			}
		}
		return nil
	})
	warnIf(r.Store.AddFindings(sha, late), "run findings not saved", "sha", c.Short)
	closedBefore := len(r.Store.OpenIssueList())
	r.resolve(sha, changes)
	r.settle(sha, findings)
	if r.Model != nil && !opt.RulesOnly { // a commit that fixed something teaches a check for the next time
		if n := r.learn(ctx, c, changes, len(r.Store.OpenIssueList()) < closedBefore, &stats); n > 0 {
			notes = append(notes, fmt.Sprintf("Codist learned %d check%s from this fix; it will look for the same mistake in later commits.", n, plural1(n)))
		}
	}
	if stats.Hidden > 0 {
		notes = append(notes, fmt.Sprintf("%d finding%s not shown because %s marked wrong before.", stats.Hidden, plural(stats.Hidden), map[bool]string{true: "it was", false: "they were"}[stats.Hidden == 1]))
	}
	if modelName != "" {
		if raw, err := json.Marshal(stats); err == nil {
			warnIf(r.Store.SetStats(sha, raw), "model usage not saved", "sha", c.Short)
		}
	}
	logger().Debug("review done", "sha", c.Short, "findings", len(findings), "hidden", stats.Hidden, "took", time.Since(started).Round(time.Millisecond))
	return r.Store.SetReview(store.Review{SHA: sha, Status: "done", Model: modelName, Note: strings.Join(notes, " "), Started: started, Finished: time.Now()})
}

// secondReading ends the prompt of a second reading. It comes last, so the model server reuses its work on
// everything before it, and it asks for another order of reading so the reply is not the first one again.
const secondReading = "\n## Second reading\nYou have read this change once. Read it again from the last changed line to the first, " +
	"and report what a first reading would miss: a wrong value, a missing case, a condition turned around.\n"

// readingName names a reading for the trail.
func readingName(extra string) string {
	if extra == secondReading {
		return "second"
	}
	if i := strings.Index(extra, "## Read as a specialist in "); i >= 0 {
		rest := extra[i+len("## Read as a specialist in "):]
		return "specialist in " + rest[:strings.Index(rest, "\n")]
	}
	return "first"
}

// Lenses are the specialist readings of a deep pass. Each ends the prompt, so the model server reuses its work on
// everything before it. One generalist reading misses what a reviewer with one question in mind sees.
var Lenses = []string{
	"\n## Read as a specialist in failures\nRead this change again with one question only: what happens when something goes wrong here? " +
		"A value that is null, empty, zero or negative; an exception that is swallowed, or thrown where it was not before; a resource left open; " +
		"a loop that runs once too often or not at all. Report only what the first reading would miss.\n",
	"\n## Read as a specialist in this team's rules\nRead this change again with one question only: does it keep every rule and document of this team " +
		"that speaks about this code? Go through the rules one by one. Check each number, limit, unit and order of steps the rules state against " +
		"the code after the commit. Report only what the first reading would miss, and name the rule.\n",
}

// NextDeep picks what a model lane with nothing else to do reads next: the newest of the last limit commits that
// the model has read but not yet read deeply, or "" when there is none. A commit whose review failed is left alone,
// so one the model cannot take is not tried for ever.
func NextDeep(st *store.Store, repo *git.Repo, limit int) string {
	log, _ := repo.LogRef("HEAD", limit)
	all, reviews := st.AllStats(), st.Reviews()
	for _, c := range log {
		var s Stats
		if raw, ok := all[c.SHA]; ok && json.Unmarshal(raw, &s) == nil && !s.Deep && s.Calls+s.Cached > 0 && reviews[c.SHA].Status == "done" {
			return c.SHA
		}
	}
	return ""
}

// addNew adds to a file's findings those of a second reading that are not near one already there.
func addNew(got, again []store.Finding) []store.Finding {
	for _, a := range again {
		if !slices.ContainsFunc(got, func(g store.Finding) bool { return a.Line >= g.Line-2 && a.Line <= g.EndLine+2 }) {
			got = append(got, a)
		}
	}
	return got
}

// finding builds a stored finding with the key that identifies the same issue in later commits:
// the rule or title, the file, and the flagged line with spacing removed, so moving code up or
// down keeps the same issue.
func finding(sha, path string, f Parsed, line, end int, severity, source, rule, title, detail, suggestion, doubt string) store.Finding {
	snippet := ""
	if lines := f.Lines(); line >= 1 && line <= len(lines) {
		snippet = strings.TrimSpace(lines[line-1])
	}
	what := rule
	if what == "" {
		what = strings.ToLower(title)
	}
	h := sha1.Sum([]byte(what + "\x00" + path + "\x00" + squash(snippet)))
	return store.Finding{SHA: sha, Path: path, Line: line, EndLine: max(end, line), Severity: severity, Source: source, Rule: rule,
		Title: title, Detail: detail, Suggestion: suggestion, Snippet: snippet, Doubt: doubt, Key: hex.EncodeToString(h[:8])}
}

var space = regexp.MustCompile(`\s+`)

func squash(s string) string { return space.ReplaceAllString(s, "") }

// sameMarks is code as the model's quotes are compared with it: without spacing and without quotation marks. A model
// writing JSON is told to type ' where the code has ", because one unescaped " ends its text early and the rest of
// the reply is lost. Some models then type ' in its place, and some leave the mark out: Qwen 3.5 quoted
// FORMULA_STARTS = "=+@"; as FORMULA_STARTS = =+@;. Both have to match.
func sameMarks(s string) string {
	return strings.NewReplacer(`"`, "", "'", "", "`", "", `\`, "").Replace(squash(s))
}

// resolve closes open issues in the files a commit changed.
func (r *Reviewer) resolve(sha string, changes []git.FileChange) {
	for _, c := range changes {
		open := r.Store.OpenIssues(c.Path)
		if len(open) == 0 {
			continue
		}
		var src []byte
		if c.Status != "deleted" {
			var ok bool
			if src, ok = r.Repo.FileAt(sha, c.Path); !ok {
				logger().Warn("file not readable, its issues stay open", "sha", sha[:min(7, len(sha))], "path", c.Path)
				continue
			}
		}
		// Only issues brought before this commit can be fixed by it. Catching up newest first, an older commit is
		// reviewed after newer ones, and the issues they brought are not in its code yet: that is not a fix.
		var fixed []string
		for _, k := range r.gone(sha, c.Path, src, c.Status != "deleted", open) {
			if i := slices.IndexFunc(open, func(o store.OpenIssue) bool { return o.Key == k }); open[i].FirstSHA == BaselineSHA || r.Repo.IsAncestor(open[i].FirstSHA, sha) {
				fixed = append(fixed, k)
			}
		}
		r.close(sha, c.Path, fixed)
	}
}

// settle closes the issues this commit opened that a later commit has already fixed. Codist reads its backlog newest
// first, so the commit that fixed an issue may have been reviewed before the one that brought it, when there was
// nothing yet to close. Only this commit's issues are settled: one opened later may simply not exist yet at an
// earlier commit.
func (r *Reviewer) settle(sha string, findings []store.Finding) {
	mine := map[string]map[string]bool{} // path -> keys of the issues this commit opened there
	for _, f := range findings {
		if f.SHA == sha && f.Key != "" {
			if mine[f.Path] == nil {
				mine[f.Path] = map[string]bool{}
			}
			mine[f.Path][f.Key] = true
		}
	}
	for path, keys := range mine {
		var open []store.OpenIssue
		for _, o := range r.Store.OpenIssues(path) {
			if keys[o.Key] {
				open = append(open, o)
			}
		}
		for _, later := range r.Repo.Later(sha, path) {
			if len(open) == 0 {
				break
			}
			src, present := r.Repo.FileAt(later, path) // gone when that commit deleted or moved the file
			fixed := r.gone(later, path, src, present, open)
			r.close(later, path, fixed)
			open = slices.DeleteFunc(open, func(o store.OpenIssue) bool { return slices.Contains(fixed, o.Key) })
		}
	}
}

// gone lists which open issues of a file are no longer there in src, the file at sha (present false when it is not
// there at all). A check issue is gone when the check, run on the whole file, no longer reports it. A model issue is
// gone when the line it flagged is gone, since the model cannot be asked again for every commit; so is a changed
// number that a document disagrees with.
func (r *Reviewer) gone(sha, path string, src []byte, present bool, open []store.OpenIssue) []string {
	var text string
	still := map[string]bool{}
	if present {
		text = squash(string(src))
		lang := r.langOf(path)
		if lang == nil {
			return nil
		}
		f := lang.Parse(path, src)
		for _, is := range lang.Check(f, nil, r.facts(lang)) {
			still[finding(sha, path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, "", "", "").Key] = true
		}
	}
	var fixed []string
	for _, o := range open {
		byLine := o.Rule == "" || o.Rule == specRule || o.Rule == ranRule // no whole-file check reports these again
		if !byLine && !still[o.Key] || byLine && squash(o.Snippet) != "" && !strings.Contains(text, squash(o.Snippet)) {
			fixed = append(fixed, o.Key)
		}
	}
	return fixed
}

// close marks issues fixed by the commit sha.
func (r *Reviewer) close(sha, path string, fixed []string) {
	if len(fixed) == 0 {
		return
	}
	logger().Debug("issues fixed", "sha", sha[:min(7, len(sha))], "path", path, "count", len(fixed))
	warnIf(r.Store.Resolve(fixed, sha), "fixed issues not saved", "sha", sha[:min(7, len(sha))], "path", path)
}

// BaselineSHA names the first-run scan of the whole code, which is stored like a review.
const BaselineSHA = "baseline"

// Baseline checks every file in full and records what it finds as open issues, each credited to
// the commit that last changed its line. It returns how many it found.
func (r *Reviewer) Baseline(files map[string][]byte) (int, error) {
	started := time.Now()
	var findings []store.Finding
	type file struct {
		path   string
		lang   Language
		parsed Parsed
	}
	var all []file
	byLang := map[Language][]Parsed{}
	for path, src := range files {
		if lang := r.langOf(path); lang != nil && (r.Ignored == nil || !r.Ignored(path)) {
			p := lang.Parse(path, src)
			all = append(all, file{path, lang, p})
			if !lang.IsTest(path) {
				byLang[lang] = append(byLang[lang], p)
			}
		}
	}
	facts := map[Language]any{}
	for lang, ps := range byLang {
		facts[lang] = lang.Facts(ps)
	}
	for _, x := range all {
		path, f := x.path, x.parsed
		var blame []string // the commit that last changed each line, read once for a file that has findings
		for _, is := range x.lang.Check(f, nil, facts[x.lang]) {
			fd := finding(BaselineSHA, path, f, is.Line, is.End, is.Severity, "rule", is.Rule, is.Title, is.Detail, is.Suggestion, "")
			if blame == nil {
				blame = append([]string{}, r.Repo.BlameAt("HEAD", path, 1, max(1, len(f.Lines())))...)
			}
			// A line not committed yet is blamed on a commit of all zeros: it has no first commit.
			if is.Line >= 1 && is.Line <= len(blame) && strings.Trim(blame[is.Line-1], "0") != "" {
				fd.FirstSHA = blame[is.Line-1]
			}
			findings = append(findings, fd)
		}
	}
	logger().Debug("baseline checked", "files", len(files), "findings", len(findings), "took", time.Since(started).Round(time.Millisecond))
	if err := r.Store.SetReview(store.Review{SHA: BaselineSHA, Status: "running", Started: started}); err != nil {
		return 0, err
	}
	if err := r.Store.ReplaceFindings(BaselineSHA, findings); err != nil {
		return 0, err
	}
	return len(findings), r.Store.SetReview(store.Review{SHA: BaselineSHA, Status: "done", Note: "Checked the whole code on first run.", Started: started, Finished: time.Now()})
}

func plural1(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func plural(n int) string {
	if n == 1 {
		return " was"
	}
	return "s were"
}

// withDeletions adds to a file's added lines the lines on either side of each place where lines were only deleted,
// so a commit that removes a check (an owner filter, a null test) is still reviewed there.
func withDeletions(added []int, patch string) []int {
	seen := map[int]bool{}
	for _, l := range added {
		seen[l] = true
	}
	out := append([]int(nil), added...)
	mark := func(l int) {
		if l >= 1 && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	n, deleting := 0, false
	for _, l := range strings.Split(patch, "\n") {
		if deleting && !strings.HasPrefix(l, "-") { // a run of deleted lines ends: pure deletion unless lines replace it
			if !strings.HasPrefix(l, "+") {
				mark(n - 1)
				mark(n)
			}
			deleting = false
		}
		switch {
		case strings.HasPrefix(l, "@@"):
			if m := newStart.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case n == 0: // file headers before the first hunk
		case strings.HasPrefix(l, "-"):
			deleting = true
		case strings.HasPrefix(l, "\\"):
		default: // added or unchanged
			n++
		}
	}
	slices.Sort(out)
	return out
}

var newStart = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)

var factsCache sync.Map // repository root and language -> cachedFacts

type cachedFacts struct {
	head  string
	facts any
}

// facts is what a language's checks know about the whole project, such as which methods return an Optional. It is
// read from HEAD once per HEAD: the newest code is the best guess at the project's methods for any commit.
func (r *Reviewer) facts(lang Language) any {
	head := r.head
	if head == "" {
		head = r.Repo.Head()
	}
	key := r.Repo.Root + "\x00" + lang.Name()
	if c, ok := factsCache.Load(key); ok && c.(cachedFacts).head == head {
		return c.(cachedFacts).facts
	}
	files, err := r.Repo.Files(head)
	if err != nil {
		logger().Warn("project facts not read; checks see one file at a time", "lang", lang.Name(), "err", err)
		return nil
	}
	var paths []string
	for _, p := range files {
		if lang.Owns(p) && !lang.IsTest(p) && (r.Ignored == nil || !r.Ignored(p)) {
			paths = append(paths, p)
		}
	}
	var parsed []Parsed
	for p, src := range r.Repo.ReadAll(head, paths) {
		parsed = append(parsed, lang.Parse(p, src))
	}
	facts := lang.Facts(parsed)
	factsCache.Store(key, cachedFacts{head, facts})
	logger().Debug("project facts", "lang", lang.Name(), "head", head[:min(7, len(head))], "files", len(parsed))
	return facts
}
