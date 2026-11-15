package activity

import (
	"fmt"
	"strings"
	"time"
)

// Dashboard is the view for someone who does not know Codist: plain words, no tokens or lanes. It says what is
// happening now, which of the steps a commit goes through it is on, how far the AI has got, what each recent commit
// brought, and how hard the machine is working, in words a person would use. A word is about four thirds of a
// token, so token counts are shown as words.
func Dashboard(s Snapshot, w, h int) string {
	w = max(60, w)
	var rows []string
	add := func(r ...string) { rows = append(rows, r...) }
	clock := s.Now.Format("15:04")
	if s.Err != "" {
		add(line(bold+cyan+" ◆ CODIST"+reset, clock, w), "", " "+red+s.Err+reset, "", dim+" q quit"+reset)
		return strings.Join(fit(rows, h), "\n")
	}
	ai := green + "● ready" + reset
	if s.Status.LLMProblem != "" {
		ai = red + "● not reachable" + reset
	}
	add(line(bold+cyan+" ◆ CODIST"+reset+"   watching "+bold+s.Status.Repo+reset+dim+" (branch "+s.Status.Branch+")"+reset,
		dim+"AI "+reset+s.Status.Model+" "+ai+"   "+dim+clock+reset, w), "")

	// What is happening now, in one sentence, and on which step.
	lane := map[string]LaneView{}
	for _, l := range s.Lanes {
		lane[l.Name] = l
	}
	message := func(sha string) string {
		for _, c := range s.Commits {
			if c.Commit.SHA == sha || strings.HasPrefix(c.Commit.SHA, sha) && sha != "" {
				return c.Commit.Message
			}
		}
		return short(sha)
	}
	step, headline, sub := 0, "All caught up. Codist is waiting for the next commit.", ""
	c := s.Running
	switch {
	case c != nil:
		what := map[string]string{
			"review":              "The AI is reading a commit",
			"second look":         "The AI is double-checking a problem it found",
			"second look, choice": "The AI is double-checking a problem it found",
			"plan":                "The AI is choosing which files of a big commit to read",
			"intent":              "The AI is asking whether changed results were meant",
		}[c.Kind]
		if what == "" {
			what = "The AI is working on " + c.Kind
		}
		step = map[string]int{"review": 3, "plan": 3, "second look": 4, "second look, choice": 4, "intent": 2}[c.Kind]
		headline = what
		sub = "“" + message(c.SHA) + "”"
		if c.Path != "" {
			sub += dim + "  ·  file " + reset + base(c.Path)
		}
		if c.Kind == "second look" || c.Kind == "second look, choice" {
			sub += dim + "  ·  the claim: " + reset + c.Title
		}
	case lane["Behaviour runs"].Running != "":
		step, headline, sub = 2, "Codist is running the changed code, before and after the commit", "“"+message(lane["Behaviour runs"].Running)+"”"
	case lane["Code checks"].Running != "":
		step, headline, sub = 1, "Codist is checking a commit for known mistakes", "“"+message(lane["Code checks"].Running)+"”"
	}
	add(" " + bold + "NOW" + reset + "   " + bold + headline + reset)
	if sub != "" {
		add("       " + sub)
	}
	if c != nil {
		elapsed := s.Now.Sub(time.UnixMilli(c.At))
		words := (len(c.System) + len(c.User)) / 5 // about five characters a word
		if c.Phase == "reading" || c.ReplyTokens == 0 {
			add("       " + yellow + "◉" + reset + fmt.Sprintf(" reading about %s words of code and rules", num(words)) + dim + "  ·  " + dur(elapsed) + " so far" + reset)
		} else {
			written := c.ReplyTokens * 3 / 4
			add("       " + green + "◉" + reset + fmt.Sprintf(" writing its answer: %s words so far ", num(written)) + bar(float64(c.ReplyTokens)/maxReply, 16) + dim + "  ·  " + dur(elapsed) + reset)
		}
	}
	add("")
	think(s, w, add)

	// The steps a commit goes through.
	steps := []string{"Quick checks", "Run the code", "AI reads it", "AI double-checks", "Result"}
	var strip strings.Builder
	strip.WriteString(" ")
	for i, name := range steps {
		n := i + 1
		mark, colour := "○", dim
		switch {
		case step == 0 && n == 5, step > n:
			mark, colour = "✓", green
		case step == n:
			mark, colour = "◉", yellow+bold
		}
		strip.WriteString(colour + mark + " " + name + reset)
		if n < len(steps) {
			strip.WriteString(dim + " ── " + reset)
		}
	}
	add(" "+dim+"HOW A COMMIT IS CHECKED"+reset, strip.String(),
		dim+"   known mistakes      before vs after      looks for bugs      makes it prove each one      issue or note"+reset, "")

	// How far the AI has got: the commits it has read, against those plus the ones it still has to read.
	read, recent := 0, min(len(s.Commits), 8)
	for _, r := range s.Commits {
		if r.Model != "" && r.Status == "done" {
			read++
		}
	}
	ml := lane["AI model"]
	busy := 0
	if ml.Running != "" {
		busy = 1
	}
	total := read + ml.Count + busy
	left := ""
	if n := len(ml.Finished); n > 0 && ml.Count+busy > 0 {
		var took int64
		for _, f := range ml.Finished {
			took += f.TookMs
		}
		left = dim + "  ·  about " + dur(time.Duration(took/int64(n)*int64(ml.Count+busy))*time.Millisecond) + " left" + reset
	}
	add(fmt.Sprintf(" %sPROGRESS%s   the AI has read %s%d of %d commits%s ", bold, reset, bold, read, total, reset)+
		bar(float64(read)/float64(max(1, total)), 20)+dim+fmt.Sprintf("  ·  %d waiting", ml.Count)+reset+left, "")

	// What was found.
	problems, serious, clean := 0, 0, 0
	for _, r := range s.Commits[:recent] {
		n := 0
		for sev, k := range r.Counts {
			n += k
			if sev == "critical" || sev == "high" {
				serious += k
			}
		}
		problems += n
		if n == 0 && r.Status == "done" && r.Model != "" { // clean only once the AI has read it too
			clean++
		}
	}
	add(fmt.Sprintf(" %sFOUND%s   %s%d problem%s%s (%d serious)   %s%d clean commit%s%s   %d fixed since",
		bold, reset, map[bool]string{true: yellow + bold, false: green + bold}[problems > 0], problems, plural(problems), reset, serious,
		green+bold, clean, plural(clean), reset, fixedCount(s.Commits[:recent])), "")

	// Each recent commit, in a sentence.
	add(" " + bold + "RECENT COMMITS" + reset)
	for _, r := range s.Commits[:recent] {
		n := 0
		for _, k := range r.Counts {
			n += k
		}
		msg := cut(r.Commit.Message, 42)
		var mark, says string
		switch {
		case n > 0:
			mark = yellow + "⚠" + reset
			says = yellow + fmt.Sprintf("%d problem%s", n, plural(n)) + reset
			if t := firstIssue(s.Issues, r.Commit.SHA); t != "" {
				says += dim + ": " + reset + t
			}
		case c != nil && c.SHA != "" && strings.HasPrefix(r.Commit.SHA, c.SHA):
			mark, says = yellow+"◉"+reset, "the AI is on it now"
		case r.Status == "queued":
			mark, says = dim+"○"+reset, dim+"waiting for the quick checks"+reset
		case r.Model == "":
			mark, says = green+"✓"+reset, "no known mistakes"+dim+"  ·  the AI has not read it yet"+reset
		default:
			mark, says = green+"✓"+reset, green+"no problems found"+reset
		}
		if r.Fixed > 0 {
			says += dim + fmt.Sprintf("  ·  fixes %d earlier", r.Fixed) + reset
		}
		add(cut(" "+mark+" "+pad(msg, 44)+says, w))
	}
	add("")

	// How hard the machine is working, in plain words.
	var calls int
	var took, prefill, decode int64
	var out int
	for _, k := range s.Calls {
		if k.Status == "running" {
			continue
		}
		calls++
		took += k.TookMs
		prefill += k.PrefillMs
		decode += k.DecodeMs
		out += k.ReplyTokens
	}
	speed := ""
	if decode > 0 {
		speed = fmt.Sprintf("  ·  writes about %.0f words a second", float64(out)*0.75/(float64(decode)/1000))
	}
	add(fmt.Sprintf(" %sTHE AI SO FAR%s   %d answer%s in %s%s", bold, reset, calls, plural(calls), dur(time.Duration(took)*time.Millisecond), speed))
	if prefill+decode > 0 {
		add(dim + fmt.Sprintf("   %d%% of that time is spent reading code, %d%% writing", prefill*100/(prefill+decode), decode*100/(prefill+decode)) + reset)
	}
	add("", dim+" d technical details · q quit"+reset)
	return strings.Join(fit(rows, h), "\n")
}

func firstIssue(is []IssueRow, sha string) string {
	for _, i := range is {
		if i.FirstSeen == sha {
			return i.Title + dim + " (" + base(i.Path) + fmt.Sprintf(":%d)", i.Line) + reset
		}
	}
	return ""
}

func fixedCount(cs []CommitRow) int {
	n := 0
	for _, c := range cs {
		n += c.Fixed
	}
	return n
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// think shows what the AI was given, what it understands and what it is deciding, for the call under way; then why
// it made its last decision.
func think(s Snapshot, w int, add func(...string)) {
	say := func(label, text string) { // wrapped onto up to three lines, so a sentence can be read whole
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		lines := wrap(text, w-22, 3)
		for i, l := range lines {
			if i > 0 {
				label = ""
			}
			add(cut("   "+dim+pad(label, 18)+reset+l, w))
		}
	}
	if c := s.Running; c != nil {
		add(" " + bold + "WHAT THE AI IS THINKING" + reset)
		if g := given(c.System, c.User); len(g) > 0 {
			say("it was given", strings.Join(g, ", "))
		}
		f, titles := fields(c.Response)
		switch {
		case strings.HasPrefix(c.Kind, "second look"):
			say("the claim", c.Title)
			say("its reasoning", f["reason"])
			if f["proves"] != "" {
				say("code it points at", f["proves"])
			}
		case c.Kind == "intent":
			say("the question", "the code now gives other results: does the commit message say that was meant?")
			say("its reasoning", f["reason"])
		default:
			say("what changed", f["change"])
			say("who notices", f["effect"])
			say("the rules say", f["expected"])
			if len(titles) > 0 {
				say("problems so far", strings.Join(titles, "; "))
			}
		}
		if c.Response == "" {
			say("", dim+"it has not started writing yet"+reset)
		}
		add("")
	}
	if c := s.LastRead; c != nil {
		f, titles := fields(c.Response)
		add(" " + bold + "LAST FILE IT READ" + reset + dim + "   " + short(c.SHA) + " " + base(c.Path) + reset)
		say("it understood", f["change"])
		say("who notices", f["effect"])
		say("the rules say", f["expected"])
		if len(titles) == 0 {
			say("it reported", green+"no problem in this file"+reset)
		} else {
			say("it reported", yellow+strings.Join(titles, "; ")+reset+dim+"  (then checked: see the commit page)"+reset)
		}
		add("")
	}
	if c := s.LastJudge; c != nil {
		f, _ := fields(c.Response)
		verdict := ""
		switch {
		case f["right"] != "":
			if choiceSide(c.User, f["right"]) == "claim" {
				verdict = green + "kept the problem" + reset
			} else {
				verdict = yellow + "sided with the code" + reset
			}
		case f["verdict"] == "reviewer_is_right":
			verdict = green + "the problem is real" + reset
		case f["verdict"] == "reviewer_is_wrong":
			verdict = yellow + "the claim is wrong" + reset
		case f["verdict"] != "":
			verdict = dim + "could not tell" + reset
		}
		add(" " + bold + "LAST DOUBLE-CHECK" + reset + dim + "   " + short(c.SHA) + " " + base(c.Path) + reset)
		say("the claim", c.Title)
		say("its decision", verdict)
		say("because", f["reason"])
		add("")
	}
}

// wrap splits text into lines of at most w columns, at most n lines, the last ending in … when text is longer.
func wrap(text string, w, n int) []string {
	var out []string
	var cur strings.Builder
	for _, word := range strings.Fields(text) {
		if cur.Len() > 0 && visible(cur.String())+1+visible(word) > w {
			out = append(out, cur.String())
			cur.Reset()
			if len(out) == n {
				out[n-1] = cut(out[n-1]+" "+word, w)
				if !strings.HasSuffix(out[n-1], "…"+reset) {
					out[n-1] += "…"
				}
				return out
			}
		}
		if cur.Len() > 0 {
			cur.WriteString(" ")
		}
		cur.WriteString(word)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
