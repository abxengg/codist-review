package review

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/abxengg/codist-git"
	"github.com/abxengg/codist-model"
	"github.com/abxengg/codist-review/store"
)

// JudgeVersion names the second look's instructions, so cached verdicts from older wording are not reused.
const JudgeVersion = "judge-9"

// The second look is one question: two reviewers, one with the claim and one saying the code is fine, and the
// judge names the one who is right, asked in both orders. It used to start with a yes-or-no question ("is the
// reviewer right?"). Qwen 3.5 answered yes 45 times in 49 on the three labs (2026-11-26) and confirmed a claim and
// its opposite 14 times in 15 (2026-11-15), as research on models checking themselves predicts (Huang et al., ICLR
// 2024; Stechly et al., ICLR 2025; Sharma et al., ICLR 2024). Worse, its "could not tell", or a failed call, kept a
// finding as an alarm without the choice: both clean-commit alarms of the lab replays came through that way. With
// every serious finding put to the choice, the replay found 60 of 62 planted bugs and alarmed 0 of 14 clean commits,
// against 59 and 2 with the yes-or-no step. A version that also made the judge quote proof did worse (57 of 62): it
// sided with the code on real bugs whose proof it said it could not see. The choice's wording is judge-9's, unchanged.

var typeName = regexp.MustCompile(`\b[A-Z][a-z]\w*\b`)

// secondLook checks each serious finding the model made on one file by making the model choose between the claim and
// the code. A claim it sides with stays an alarm; one it sides against, or cannot decide on, is kept as a note with
// the reason, so a person can still see it. Notes and low findings are left as they are: they raise no alarm.
func (r *Reviewer) secondLook(ctx context.Context, c git.Commit, ch git.FileChange, after []string, got []store.Finding, st *Stats) []store.Finding {
	var before []string
	if len(c.Parents) > 0 && ch.Status != "added" {
		if src, ok := r.Repo.FileAt(c.Parents[0], cmpOr(ch.OldPath, ch.Path)); ok {
			before = strings.Split(string(src), "\n")
		}
	}
	// The team's rules go with the instructions, at the very start, and not after the code: the model server reads a
	// prompt that starts like the last one in about a second, and one that differs from its first words in full.
	// Measured on the 14b model: 1,400 tokens in 1.1 s against 1,600 in 35 s.
	rules := ""
	if r.Rules != "" {
		rules = "\n\n## The team's rules\n" + clip(r.Rules, 3000)
	}
	for i, f := range got {
		if f.Doubt != "" || f.Severity == "low" || f.Severity == "info" {
			continue
		}
		prompt, _ := r.judgePrompt(c, ch, after, before, f)
		side, why := r.choose(ctx, c, ch, f, rules, prompt, st)
		doubt := ""
		switch side {
		case "fine":
			doubt = "A second look, made to choose, sided with the code: " + why
		case "undecided":
			doubt = "A second look could not choose between this claim and its opposite, so it settles nothing."
		case "claim", "split":
			if side == "split" && slices.Index(severities, got[i].Severity) < slices.Index(severities, "medium") {
				got[i].Severity = "medium"
			}
		default: // the model did not answer: the finding stays as it was
			continue
		}
		logger().Debug("second look", "sha", c.Short, "path", ch.Path, "line", f.Line, "title", f.Title, "side", side, "doubt", doubt)
		if doubt != "" {
			got[i].Doubt, got[i].Severity = doubt, "low"
			st.Refuted++
		}
	}
	return got
}

const chooseSystem = `Two code reviewers disagree about one place in a commit. You are shown the code before and
after the commit, the definitions of the project types it uses, and the team's rules. Decide only from the code shown.
A problem is something the program does wrong when it runs: a wrong result, a crash, lost data, a broken rule of the
team. Exactly one of the two reviewers is right. Reply with JSON only: "reason", one plain sentence that names the
line of code or the rule that decides it, then "right": "A" or "B". Never type a double quote inside the reason.`

var chooseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"reason": text(300),
		"right":  map[string]any{"type": "string", "enum": []string{"A", "B"}},
	},
	"required": []string{"reason", "right"},
}

const codeIsFine = "The code after the commit is correct at this place: it breaks no rule of the team and nothing goes wrong when it runs. The other reviewer is mistaken."

// choose makes the judge pick between the claim and its opposite, twice, with the two reviewers in either order
// (position bias: Wang et al., ACL 2024; Zheng et al., 2023). It answers "claim" when the judge picks the claim both
// times, "fine" when it picks the code both times (with its reason), "split" when its answers follow the order but
// both reasons speak of the claim, "undecided" when they follow the order otherwise, and "failed" when the model did
// not answer. why is the judge's last reason.
func (r *Reviewer) choose(ctx context.Context, c git.Commit, ch git.FileChange, f store.Finding, rules, prompt string, st *Stats) (side, why string) {
	claim := strings.TrimSpace(f.Title + ". " + f.Detail)
	picked, reasons := [2]string{}, [2]string{}
	for i, order := range [2][2]string{{claim, codeIsFine}, {codeIsFine, claim}} {
		var out struct {
			Reason string `json:"reason"`
			Right  string `json:"right"`
		}
		// The code comes first and the two reviewers last, so the second question reads only its last lines anew.
		q := prompt[strings.Index(prompt, "## Commit"):] + fmt.Sprintf("\n## The disagreement\nFile %s, line %d.\nReviewer A says: %s\nReviewer B says: %s\n", ch.Path, f.Line, order[0], order[1])
		key := cacheKey(r.Model.Name, JudgeVersion+"\x00choose\x00"+chooseSystem+rules+"\x00"+q)
		if raw, ok := r.Store.CachedReply(key); ok && json.Unmarshal(raw, &out) == nil {
			st.Cached++
		} else {
			u, err := r.Model.ChatJSON(model.WithLabel(ctx, model.Label{Kind: "second look, choice", Title: f.Title, SHA: c.Short, Path: ch.Path}),
				chooseSystem+rules, q, chooseSchema, &out)
			if err != nil {
				logger().Warn("second look failed; finding kept as it was", "sha", c.Short, "path", ch.Path, "line", f.Line, "err", err)
				return "failed", ""
			}
			st.add(u)
			if raw, err := json.Marshal(out); err == nil {
				warnIf(r.Store.SetCachedReply(key, raw), "choice not cached", "sha", c.Short)
			}
		}
		picked[i] = map[bool]string{true: "claim", false: "fine"}[(out.Right == "A") == (i == 0)]
		r.trail.step(f.Key, "choice", map[string]string{"claim": "sided with the claim", "fine": "sided with the code"}[picked[i]],
			"%s the claim listed %s. %s", map[bool]string{true: "Asked with", false: "Asked again with"}[i == 0], map[bool]string{true: "first", false: "second"}[i == 0], strings.TrimSpace(out.Reason))
		why = strings.TrimSpace(out.Reason)
		reasons[i] = why
		logger().Debug("second look, choice", "sha", c.Short, "path", ch.Path, "line", f.Line, "title", f.Title, "order", i, "right", out.Right, "picked", picked[i], "reason", out.Reason)
	}
	if picked[0] != picked[1] {
		// Answers that follow the order, with both reasons about what the claim says: the judge argued the point
		// and only its letter followed the order. On the three labs (2026-11-26) this demoted 3 real bugs and no
		// finding on a clean commit, and earlier runs lost the records-lab CSV bug the same way, so it stands, at
		// medium at most, for a person to judge (Wang et al. send such cases to a human).
		if onTopic(claim, reasons[0]) && onTopic(claim, reasons[1]) {
			r.trail.step(f.Key, "choice", "split, kept", "Its answers followed the order, but both of its reasons spoke of the claim, so the claim stands.")
			return "split", why
		}
		return "undecided", ""
	}
	// A choice for the claim counts only when the judge's reasons speak of what the claim says. Qwen 3.5 picked
	// "non-deterministic lookup on duplicate department codes" twice for reasons about head counts and personal data.
	if picked[0] == "claim" && !(onTopic(claim, reasons[0]) && onTopic(claim, reasons[1])) {
		r.trail.step(f.Key, "choice", "off the point", "Its reasons for the claim were about something else.")
		return "undecided", ""
	}
	return picked[0], why
}

// usual are words that say nothing about what a claim is about.
var usual = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`about above after again against because before being below between called calls change changed
		changes check claim class commit could does every first would should their there these those through under value values
		return returns rules reviewer still where which while other method methods problem right since thing which without`) {
		usual[w] = true
	}
}

var claimWord = regexp.MustCompile(`[A-Za-z][A-Za-z_]{4,}`)

// onTopic says a reason speaks of the claim: it shares a telling word with it (two when the claim has several),
// matched on the first five letters, so "quoted" meets "quoting".
func onTopic(claim, reason string) bool {
	stem := func(w string) string { return strings.ToLower(w)[:5] }
	in := map[string]bool{}
	for _, w := range claimWord.FindAllString(reason, -1) {
		in[stem(w)] = true
	}
	seen, shared := map[string]bool{}, 0
	for _, w := range claimWord.FindAllString(claim, -1) {
		s := stem(w)
		if usual[strings.ToLower(w)] || seen[s] {
			continue
		}
		seen[s] = true
		if in[s] {
			shared++
		}
	}
	return shared >= min(2, len(seen))
}

// judgePrompt is what the judge is asked, and shown: the code in it as written, to check the judge's quotes against.
func (r *Reviewer) judgePrompt(c git.Commit, ch git.FileChange, after, before []string, f store.Finding) (prompt, shown string) {
	var b, code strings.Builder
	fmt.Fprintf(&b, "## The claim\nFile %s, line %d, severity %s.\n%s\n%s\n\n", ch.Path, f.Line, f.Severity, f.Title, f.Detail)
	fmt.Fprintf(&b, "## Commit\n%s\n\n", c.Subject)
	from, to := max(1, f.Line-25), min(len(after), f.EndLine+25)
	fmt.Fprintf(&b, "## %s after the commit, lines %d to %d\n```\n", ch.Path, from, to)
	for l := from; l <= to; l++ {
		fmt.Fprintf(&b, "%4d| %s\n", l, after[l-1])
		code.WriteString(after[l-1] + "\n")
	}
	b.WriteString("```\n\n")
	if len(before) > 0 {
		b.WriteString("## The same part before the commit\n```diff\n")
		b.WriteString(clip(ch.Patch, 3000))
		for _, l := range strings.Split(clip(ch.Patch, 3000), "\n") {
			if l != "" && strings.ContainsRune("+- ", rune(l[0])) {
				l = l[1:]
			}
			code.WriteString(l + "\n")
		}
		b.WriteString("```\n\n")
	} else {
		b.WriteString("## Before the commit\nThis file is new in this commit.\n\n")
	}
	// The types the claim itself names come first: a claim about what Paging.of does with null cannot be judged
	// without Paging, and the code around the line can name three other types before it.
	window := f.Title + "\n" + f.Detail + "\n" + strings.Join(after[from-1:to], "\n")
	for _, d := range r.typeDefinitions(c.SHA, ch.Path, window) {
		b.WriteString(d)
		code.WriteString(d)
	}
	return b.String(), code.String()
}

// typeDefinitions are the declarations of up to four project types named in the claim or near it, other than the file's
// own, so the judge can see what a type really holds instead of guessing.
func (r *Reviewer) typeDefinitions(sha, self, text string) []string {
	files, err := r.Repo.Files(sha)
	if err != nil {
		return nil
	}
	lang := r.langOf(self)
	byName := map[string]string{}
	for _, p := range files {
		if lang != nil && lang.Owns(p) && !lang.IsTest(p) && p != self {
			name := strings.TrimSuffix(path.Base(p), path.Ext(p))
			byName[name] = p
		}
	}
	var out []string
	var seen []string
	for _, name := range typeName.FindAllString(text, -1) {
		p, ok := byName[name]
		if !ok || slices.Contains(seen, name) || len(out) == 4 {
			continue
		}
		seen = append(seen, name)
		if src, ok := r.Repo.FileAt(sha, p); ok {
			out = append(out, fmt.Sprintf("## Definition of %s (%s)\n```\n%s```\n\n", name, p, clip(string(src), 2500)))
		}
	}
	return out
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "\n… (cut)\n"
	}
	return s
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
