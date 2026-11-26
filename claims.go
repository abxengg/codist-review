package review

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A model is checked like a new colleague: it must quote the code it means, the quote must be
// near the line it names, and a claim the code cannot back is shown as unconfirmed rather than
// stated as fact. In our measurements, dropping weak claims cost almost no real catches.

var numbered = regexp.MustCompile(`^\s*(>>)?\s*\d*\s*\|?\s*`)

// normalise strips line numbers and all spacing, so a quote that joins a multi-line statement
// still matches.
func normalise(quote string) string {
	var parts []string
	for _, l := range strings.Split(quote, "\n") {
		parts = append(parts, numbered.ReplaceAllString(l, ""))
	}
	return sameMarks(strings.Join(parts, ""))
}

// quoted says whether the quote appears in the file within reach lines of line.
func quoted(lines []string, quote string, line, reach int) (inFile, near bool) {
	want := normalise(quote)
	if want == "" {
		return false, false
	}
	var whole strings.Builder
	starts := make([]int, len(lines))
	for i, l := range lines {
		starts[i] = whole.Len()
		whole.WriteString(sameMarks(l))
	}
	text := whole.String()
	for from := 0; ; {
		at := strings.Index(text[from:], want)
		if at < 0 {
			return inFile, false
		}
		at += from
		inFile = true
		first := 0 // 1-based line where the match starts
		for i, s := range starts {
			if s <= at {
				first = i + 1
			}
		}
		if first >= line-reach && first <= line+reach {
			return true, true
		}
		from = at + 1
	}
}

var (
	vague       = regexp.MustCompile(`(?i)^(consider|you (may|might) want|it (may|might) be|review the|ensure that|make sure)\b`)
	changeTitle = regexp.MustCompile(`(?i)^(improved|simplified|added|adding|renamed|refactored|updated|changed|moved|replaced|introduced|extracted|explicitly passing|use of)\b`)
	nullClaim   = regexp.MustCompile(`(?i)\bnull|\bnpe\b|nullpointer`)
	// Not a claim that something crashes on null: a method that returns null instead of a value is a wrong value, and
	// NULL in capitals is SQL's. Those are ordinary claims and need no source of null.
	nullNot     = regexp.MustCompile(`(?i)returns? null instead|instead of (returning )?null|null string`)
	sqlNull     = regexp.MustCompile(`\bNULL\b`) // in capitals: SQL's, case matters
	receiver    = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*(?:\.\s*[A-Za-z_]\w*|\[)`)
	removedNull = regexp.MustCompile(`(?m)^-[^-].*\bnull\b`)
	// A removed default: getOrDefault, orElse or requireNonNullElse taken out means a missing value now comes back as
	// null (shop-lab: RATES.getOrDefault(state, 1800) became RATES.get(state), an int unboxed from null).
	removedDefault = regexp.MustCompile(`(?m)^-[^-].*(\.getOrDefault\(|\.orElse(Get)?\(|requireNonNullElse(Get)?\(|defaultIfNull\()`)
	neverNull      = regexp.MustCompile(`\.(orElseThrow|orElse|orElseGet|requireNonNull)\(|\bnew\s`)
	thisField      = regexp.MustCompile(`\bthis\.([a-z_]\w*)\b(?:\s*[^\s(]|\s*$)`)
	equalsArg      = regexp.MustCompile(`(equals|==|!=)\s*\(?\s*$`)
	// A claim that only says the commit message and the code disagree. "Mismatch" alone is not that: a path separator
	// or a type mismatch is a real problem.
	onlyMessage = regexp.MustCompile(`(?i)commit message|message (does not|doesn't|do not) match|contradicts the (commit|message)|message mismatch|mismatch (with|between) (the )?(commit )?message`)
)

var severities = []string{"critical", "high", "medium", "low", "info"}

// judge decides what happens to one model finding: drop says why it is not shown at all; otherwise
// severity and doubt are what to show.
func judge(lines []string, patch string, line int, severity, title, evidence string) (drop, newSeverity, doubt string) {
	if line < 1 || line > len(lines) {
		return "points at a line that is not in the file", "", ""
	}
	if vague.MatchString(strings.TrimSpace(title)) {
		return "vague advice", "", ""
	}
	inFile, near := quoted(lines, evidence, line, 8)
	switch {
	case strings.TrimSpace(evidence) == "":
		doubt = "The model did not quote the code it means."
	case !inFile && removedNear(patch, evidence, line, 8):
		// The code the model quotes is what the change took out: a claim that a removed check or formula is missed.
		// The review asks for exactly that, so the quote stands.
	case !inFile:
		return "quotes code that is not in the file", "", ""
	case !near:
		return "quotes code far from the line it names", "", ""
	}
	if doubt == "" {
		switch {
		case changeTitle.MatchString(strings.TrimSpace(title)):
			doubt = "This describes the change and names no problem in it."
		case nullClaim.MatchString(title) && !nullNot.MatchString(title) && !sqlNull.MatchString(title) && !nullBacked(evidence, lines, line, patch):
			doubt = "Codist could not find where this value becomes null in this code."
		case onlyMessage.MatchString(title):
			doubt = "This says the commit message and the code disagree, but not what now goes wrong."
		}
	}
	if !slices.Contains(severities, severity) {
		severity = "medium"
	}
	if doubt != "" && slices.Index(severities, severity) < slices.Index(severities, "low") {
		severity = "low"
	}
	return "", severity, doubt
}

// nullBacked says the code gives a value a way to be null: the change removed a null check, or
// nearby code tests the value for null, sets it to null, or takes it from a call that returns null
// on a miss.
func nullBacked(evidence string, lines []string, line int, patch string) bool {
	// A null error needs a value that can be null used where null fails: called with a dot, or a field handed to code
	// that needs a value (new LinkedHashMap<>(this.map)). In !HR.equals(role) or Objects.equals(a, b) nothing that can
	// be null is used that way, so the claim has nothing to stand on, whatever the change removed.
	names := usedNames(evidence)
	if len(names) == 0 {
		return false
	}
	if removedNull.MatchString(patch) || removedDefault.MatchString(patch) {
		return true
	}
	from, to := max(0, line-31), min(len(lines), line+30)
	near := strings.Join(lines[from:to], "\n")
	for _, name := range names {
		v := regexp.QuoteMeta(name)
		source := `(?m)\b` + v + `\s*[!=]=\s*null\b|\bnull\s*[!=]=\s*(this\.)?` + v + `\b|\b` + v + `\s*=\s*null\b|@Nullable\b[^;\n]*\b` + v +
			`\b|\b` + v + `\s*=[^;\n]*\.(get|find\w*|remove|poll|peek\w*|getProperty|getenv|getParameter|getHeader|getAttribute|lookup)\(`
		for _, hit := range regexp.MustCompile(source).FindAllString(near, -1) {
			if !neverNull.MatchString(lineOf(near, hit)) { // orElseThrow() or new never gives null
				return true
			}
		}
	}
	return false
}

// usedNames lists the values in code that would fail if they were null: receivers of a call or field access, and
// fields read through this (this.map), unless they are only compared with equals.
func usedNames(code string) []string {
	var out []string
	for _, m := range receiver.FindAllStringSubmatch(code, -1) {
		if name := m[1]; name != "this" && name != "super" && !(name[0] >= 'A' && name[0] <= 'Z') {
			out = append(out, name)
		}
	}
	for _, m := range thisField.FindAllStringSubmatchIndex(code, -1) {
		name, before := code[m[2]:m[3]], code[:m[0]]
		if !equalsArg.MatchString(before) {
			out = append(out, name)
		}
	}
	return out
}

func dereferences(code string) bool {
	for _, m := range receiver.FindAllStringSubmatch(code, -1) {
		name := m[1]
		if name == "this" || name == "super" || name[0] >= 'A' && name[0] <= 'Z' {
			continue // a constant or a type: static calls and constants are never null
		}
		return true
	}
	return false
}

// lineOf is the whole line of text that holds part.
func lineOf(text, part string) string {
	i := strings.Index(text, part)
	if i < 0 {
		return part
	}
	start := strings.LastIndex(text[:i], "\n") + 1
	end := strings.Index(text[i:], "\n")
	if end < 0 {
		return text[start:]
	}
	return text[start : i+end]
}

// removedNear says the quote is among the lines the change removed, at a place near line in the file after it.
func removedNear(patch, quote string, line, reach int) bool {
	want := normalise(quote)
	if want == "" {
		return false
	}
	var run strings.Builder // consecutive removed lines, so a quote spanning them still matches
	at, n := 0, 0           // where the run sits in the new file; the new file's current line
	check := func() bool {
		found := run.Len() > 0 && strings.Contains(run.String(), want) && at >= line-reach && at <= line+reach
		run.Reset()
		return found
	}
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			if check() {
				return true
			}
			if m := hunkNew.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+++"):
		case strings.HasPrefix(l, "-"):
			if run.Len() == 0 {
				at = n
			}
			run.WriteString(sameMarks(numbered.ReplaceAllString(l[1:], "")))
		default:
			if check() {
				return true
			}
			n++
		}
	}
	return check()
}

var hunkNew = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)
