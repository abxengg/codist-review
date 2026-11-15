package review

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	git "github.com/abxengg/codist-git"
	"github.com/abxengg/codist-review/store"
)

// A commit can change a number the project's own documents state: the spec says the light dims every 20 seconds and
// the commit makes it 10. A model reading the diff sees nothing wrong, since 10 is as plausible as 20, and on the
// planted-bug game it missed every such change. This check needs no model: when a commit changes one number on a
// line, it looks for a paragraph in the documents that states the old number about the same thing, and that the
// commit left as it was. Documents are never assumed right: the finding says the two disagree and a person decides.

// specRule names the check. Its issues are followed by their line, like the model's, since no whole-file check
// reports them again.
const specRule = "spec-number"

var (
	codeNumber = regexp.MustCompile(`\b\d[\d_]*(?:\.\d+)?[fFdDlL]?\b`)
	docNumber  = regexp.MustCompile(`\d+(?:\.\d+)?`)
	thousands  = regexp.MustCompile(`(\d),(\d{3})\b`)
	wordPart   = regexp.MustCompile(`[A-Z]?[a-z]+|[A-Z]+`)
	docWord    = regexp.MustCompile(`[A-Za-z]{4,}`)
	listItem   = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s|^#`)
	// words of the language, not of the thing the number is about
	plainWords = strings.Fields("public private protected static final double float long short return const this void class " +
		"math integer string boolean true false null else while throw throws import package value number count size " +
		"default with from that have must when then they")
)

// number is a number as written in code or prose, in one spelling: 20, 20.0, 20L and 2_0 are all "20".
func number(s string) string {
	s = strings.TrimRight(strings.ReplaceAll(s, "_", ""), "fFdDlL")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// numberChange is one line of a patch on which a commit changed exactly one number.
type numberChange struct {
	line     int // in the file after the commit
	old, new string
}

// numberChanges finds the lines a patch changes in one number only: a removed line and an added line in the same
// place that are the same once their numbers are taken out.
func numberChanges(patch string) []numberChange {
	var out []numberChange
	var removed []string
	n := 0
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			removed = nil
			if m := newStart.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case n == 0 || strings.HasPrefix(l, `\`):
		case strings.HasPrefix(l, "-"):
			removed = append(removed, l[1:])
		case strings.HasPrefix(l, "+"):
			if t := strings.TrimSpace(l[1:]); !strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "*") && !strings.HasPrefix(t, "/*") {
				shape, nums := codeNumber.ReplaceAllString(l[1:], "#"), codeNumber.FindAllString(l[1:], -1)
				for _, old := range removed {
					was := codeNumber.FindAllString(old, -1)
					if codeNumber.ReplaceAllString(old, "#") != shape || len(was) != len(nums) {
						continue
					}
					differ := -1
					for i := range nums {
						if number(nums[i]) != number(was[i]) {
							if differ >= 0 {
								differ = -2
								break
							}
							differ = i
						}
					}
					if differ >= 0 {
						out = append(out, numberChange{n, number(was[differ]), number(nums[differ])})
					}
					break
				}
			}
			n++
		default:
			removed = nil
			n++
		}
	}
	return out
}

// names are the words a line of code, and the file it is in, use for what they are about: DIM_EVERY in
// Lantern.java gives "every" from the line and "lantern" from the file.
func names(text string) []string {
	var out []string
	for _, w := range wordPart.FindAllString(text, -1) {
		if w = strings.ToLower(w); len(w) >= 4 && !slices.Contains(plainWords, w) && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// sameWord says two words are one word in two forms: step and steps, buffer and buffered.
func sameWord(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// paragraph is one statement in a document: a list item or a run of lines up to a blank line.
type paragraph struct {
	path string
	line int
	text string
}

func paragraphs(p string, src []byte) []paragraph {
	var out []paragraph
	var cur []string
	start := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, paragraph{p, start, strings.Join(cur, " ")})
		}
		cur = nil
	}
	fence := false
	for i, l := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fence = !fence
			flush()
			continue
		}
		if fence {
			continue
		}
		if strings.TrimSpace(l) == "" || listItem.MatchString(l) {
			flush()
		}
		if strings.TrimSpace(l) != "" {
			if len(cur) == 0 {
				start = i + 1
			}
			cur = append(cur, strings.TrimSpace(l))
		}
	}
	flush()
	return out
}

// isDoc says a file is prose about the project. Change logs tell what was, not what should be.
func isDoc(p string) bool {
	base := strings.ToLower(path.Base(p))
	return (strings.HasSuffix(base, ".md") || strings.HasSuffix(base, ".adoc") || strings.HasSuffix(base, ".txt")) &&
		!strings.HasPrefix(base, "changelog") && !strings.HasPrefix(base, "history") && !strings.HasPrefix(base, "licen") &&
		!strings.Contains(p, "node_modules/")
}

const maxDocs = 40

// docsAt reads the documents at a commit, once for a review.
func (r *Reviewer) docsAt(sha string) []paragraph {
	if r.docs != nil {
		return r.docs
	}
	r.docs = []paragraph{}
	files, err := r.Repo.Files(sha)
	if err != nil {
		return r.docs
	}
	var paths []string
	for _, p := range files {
		if isDoc(p) && len(paths) < maxDocs {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	read := r.Repo.ReadAll(sha, paths)
	for _, p := range paths {
		if src := read[p]; len(src) < 200_000 {
			r.docs = append(r.docs, paragraphs(p, src)...)
		}
	}
	return r.docs
}

// specNumbers reports the lines of one changed file where the commit changed a number that a document still states
// with the old value.
func (r *Reviewer) specNumbers(sha string, ch git.FileChange, f Parsed) []store.Finding {
	var out []store.Finding
	lines := f.Lines()
	for _, nc := range numberChanges(ch.Patch) {
		if nc.old == "0" || nc.old == "1" || nc.line < 1 || nc.line > len(lines) {
			continue // 0 and 1 are in every document
		}
		strong := names(lines[nc.line-1])
		weak := names(strings.TrimSuffix(path.Base(ch.Path), path.Ext(ch.Path)))
		need := 2 // one word from the line, or two from the file's name
		if len(nc.old) == 1 {
			need = 3 // a single digit is common in prose: ask for more
		}
		var best *paragraph
		bestScore := 0
		for i, p := range r.docsAt(sha) {
			text := thousands.ReplaceAllString(thousands.ReplaceAllString(p.text, "$1$2"), "$1$2")
			has := map[string]bool{}
			for _, n := range docNumber.FindAllString(text, -1) {
				has[number(n)] = true
			}
			if !has[nc.old] || has[nc.new] {
				continue // says nothing about the old value, or names both
			}
			score := 0
			words := docWord.FindAllString(strings.ToLower(text), -1)
			for _, w := range strong {
				if slices.ContainsFunc(words, func(d string) bool { return sameWord(d, w) }) {
					score += 2
				}
			}
			for _, w := range weak {
				if !slices.Contains(strong, w) && slices.ContainsFunc(words, func(d string) bool { return sameWord(d, w) }) {
					score++
				}
			}
			if score >= need && score > bestScore {
				best, bestScore = &r.docs[i], score
			}
		}
		if best == nil {
			continue
		}
		fd := finding(sha, ch.Path, f, nc.line, nc.line, "medium", "rule", specRule,
			fmt.Sprintf("Changes %s to %s, but %s says %s", nc.old, nc.new, best.path, nc.old),
			fmt.Sprintf("%s, line %d, says: \"%s\" This commit changes the value from %s to %s and leaves the document as it is, so the code and the document now disagree.",
				best.path, best.line, clipText(best.text, 300), nc.old, nc.new),
			"Put the value back, or change the document in the same commit if the new value is the one that is meant.", "")
		out = append(out, fd)
	}
	return out
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
