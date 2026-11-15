package review

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	git "github.com/abxengg/codist-git"
	"github.com/abxengg/codist-review/store"
)

// What running a commit's code showed is evidence no reading of a diff can give, and it is used twice.
//
// First, as findings that need no model. A commit whose message announces work that must not change results (a
// refactoring, a rename, tidying) and whose methods now return something else has broken its own promise: running
// 22 real refactoring commits showed no difference at all, so a difference under such a message is rare and telling.
// A call that returned and now crashes, or a result that was never negative and now is, is a bug or close to one
// whatever the message says.
//
// Second, as facts for the model: it is shown the calls whose result changed in the file it reads, so it can hold
// "stunBonus(3) returned 300 before and 400 after" against the team's rules instead of guessing what the code does.

// ranRule names the finding. Its issues are followed by their line, since no whole-file check reports them again.
const ranRule = "behaviour-change"

// keepsResults matches a commit message that announces work which must not change what the code returns.
var keepsResults = regexp.MustCompile(`(?i)^\s*(simplif|refactor|renam|tidy|tidi|clean ?up|extract|inline|reformat|restructur|reorgani[sz]|` +
	`dedup|no functional change)`)

// ran is the part of a behaviour report this file uses.
type ran struct {
	Differ      int `json:"differ"`
	Unintended  int `json:"unintended"`
	Differences []struct {
		Method, Call, Old, New, Intent string
	} `json:"differences"`
}

func (r *Reviewer) ranAt(sha string) *ran {
	raw, ok := r.Store.Behaviour(sha)
	if !ok {
		return nil
	}
	var rep ran
	if json.Unmarshal(raw, &rep) != nil || len(rep.Differences) == 0 {
		return nil
	}
	return &rep
}

// classFile says whether a harness method name ("pkg.Outer$Inner#name") belongs to a file.
func classFile(method, file string) bool {
	class, _, _ := strings.Cut(method, "#")
	class = class[strings.LastIndex(class, ".")+1:]
	class, _, _ = strings.Cut(class, "$")
	return class != "" && strings.TrimSuffix(path.Base(file), path.Ext(file)) == class
}

// crashes are exceptions no commit means to bring: a method that returned and now dies with one of these has a bug,
// whatever the commit message says.
var crashes = regexp.MustCompile(`^throws java\.lang\.(NullPointerException|ArrayIndexOutOfBoundsException|IndexOutOfBoundsException|` +
	`StringIndexOutOfBoundsException|ClassCastException|ArithmeticException|StackOverflowError|NegativeArraySizeException)`)

var numeric = regexp.MustCompile(`^-?\d+(\.\d+)?(E-?\d+)?$`)

// ranFindings reports what running a commit proved about its changed files, in this order, one finding a file:
//   - a call that returned before now crashes;
//   - the message says results should not change (or the model read it so), and they did;
//   - a method that never returned a negative number in the calls that differ now does: a lower bound is gone.
func (r *Reviewer) ranFindings(c git.Commit, changes []git.FileChange, parsed func(git.FileChange) Parsed) []store.Finding {
	rep := r.ranAt(c.SHA)
	if rep == nil {
		return nil
	}
	tidy := keepsResults.MatchString(c.Subject) || rep.Unintended > 0
	// A change can show only in the methods that call the changed one, which live in other files. Differences that
	// belong to no changed file are put with the first changed file of the language, so they are not lost.
	home := ""
	owned := func(method string) bool {
		for _, ch := range changes {
			if classFile(method, ch.Path) {
				return true
			}
		}
		return false
	}
	for _, ch := range changes {
		if l := r.langOf(ch.Path); home == "" && l != nil && !l.IsTest(ch.Path) && ch.Status != "deleted" && len(ch.Added) > 0 {
			home = ch.Path
		}
	}
	var out []store.Finding
	for _, ch := range changes {
		if ch.Status == "deleted" || len(ch.Added) == 0 {
			continue
		}
		var calls, crashed, negative []string
		methods := map[string]bool{}
		oldNegative := false
		for _, d := range rep.Differences {
			if !classFile(d.Method, ch.Path) && !(ch.Path == home && !owned(d.Method)) {
				continue
			}
			was, now := short(d.Old), short(d.New)
			line := fmt.Sprintf("%s returned %s before and %s after", d.Call, was, now)
			if crashes.MatchString(now) && !strings.HasPrefix(was, "throws") {
				crashed = append(crashed, fmt.Sprintf("%s returned %s before and now %s", d.Call, was, now))
			}
			if numeric.MatchString(was) && numeric.MatchString(now) {
				oldNegative = oldNegative || strings.HasPrefix(was, "-")
				if strings.HasPrefix(now, "-") && !strings.HasPrefix(was, "-") && d.Intent != "intended" {
					negative = append(negative, line)
				}
			}
			if d.Intent != "intended" {
				methods[d.Method[strings.Index(d.Method, "#")+1:]] = true
				calls = append(calls, line)
			}
		}
		var names []string
		for m := range methods {
			names = append(names, m)
		}
		who := strings.Join(sortedStrings(names), ", ")
		const ran = "Codist ran the changed code before and after this commit with the same inputs. "
		var title, detail, fix, severity, doubt string
		switch {
		case len(crashed) > 0:
			severity, title = "high", "Calls that worked before this commit now crash"
			detail = ran + strings.Join(crashed[:min(3, len(crashed))], "; ") + "."
			fix = "Handle the case that now crashes, as the code did before."
		case tidy && len(calls) > 0:
			severity, title = "high", fmt.Sprintf("The commit says %q, but %s now behaves differently", clipText(c.Subject, 60), who)
			detail = ran + strings.Join(calls[:min(3, len(calls))], "; ") + fmt.Sprintf(". %d calls differ in all. A commit described this way should change no result.", rep.Differ)
			fix = "If the new results are meant, say so in the commit message; otherwise the change has a bug."
		case len(negative) > 0 && !oldNegative:
			severity, title = "medium", "Results that were never negative now are: a lower limit may be gone"
			detail = ran + strings.Join(negative[:min(3, len(negative))], "; ") + ". In the calls whose result changed, the old code never returned a negative number."
			fix = "Check whether the change removed a floor such as Math.max, and put it back if a negative result makes no sense here."
		case len(calls) > 0:
			// The message does not say whether results should change, so this is for a person to judge: a note
			// beside the code, which opens no issue.
			severity, title = "low", fmt.Sprintf("%s now behaves differently; the commit message does not say whether that is meant", who)
			detail = ran + strings.Join(calls[:min(3, len(calls))], "; ") + fmt.Sprintf(". %d calls differ in all.", rep.Differ)
			doubt = "Running the code shows a change in results. Whether it was meant is yours to say."
		default:
			continue
		}
		if f := parsed(ch); f != nil {
			out = append(out, finding(c.SHA, ch.Path, f, ch.Added[0], ch.Added[0], severity, "rule", ranRule, title, detail, fix, doubt))
		}
	}
	return out
}

// RanFindings stores what running a commit showed as findings, when its review is already done. It is for the
// behaviour lane, which may finish after the review.
func (r *Reviewer) RanFindings(sha string) error {
	c, changes, err := r.Repo.Show(sha)
	if err != nil {
		return err
	}
	fs := r.ranFindings(c, changes, func(ch git.FileChange) Parsed {
		lang := r.langOf(ch.Path)
		src, ok := r.Repo.FileAt(c.SHA, ch.Path)
		if lang == nil || !ok {
			return nil
		}
		return lang.Parse(ch.Path, src)
	})
	if len(fs) == 0 {
		return nil
	}
	if _, reviewed := r.Store.GetReview(c.SHA); !reviewed {
		return nil // the review to come adds them itself
	}
	return r.Store.AddFindings(c.SHA, fs)
}

// ranSection tells the model what running the code showed for one file, or "".
func (r *Reviewer) ranSection(rep *ran, file string) string {
	if rep == nil {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, d := range rep.Differences {
		if classFile(d.Method, file) && n < 6 {
			if n == 0 {
				b.WriteString("\n## What running this file's methods showed\nCodist called the changed methods with the same inputs before and after this commit. " +
					"These calls now give a different result. They are facts: hold each against the commit message and the team's rules, " +
					"and report a result that a rule forbids or that the message does not announce.\n")
			}
			fmt.Fprintf(&b, "- %s: before %s, after %s\n", d.Call, short(d.Old), short(d.New))
			n++
		}
	}
	return b.String()
}

// short is a harness result without the echo of the arguments.
func short(s string) string {
	s, _, _ = strings.Cut(s, " | args")
	return clipText(strings.TrimSpace(s), 80)
}

func sortedStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}
