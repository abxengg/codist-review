package activity

import (
	"regexp"
	"strings"
)

// What the AI thinks is in its reply, which is JSON written a piece at a time. These read the fields out of a reply
// that may still be unfinished, so the dashboard can show its thinking while it writes.

var field = regexp.MustCompile(`"(change|effect|expected|reason|title|verdict|right|clears|proves)"\s*:\s*"((?:[^"\\]|\\.)*)`)

// fields are the text fields of a reply, finished or not, in the order written; titles are the findings' titles.
func fields(reply string) (f map[string]string, titles []string) {
	f = map[string]string{}
	for _, m := range field.FindAllStringSubmatch(reply, -1) {
		v := strings.NewReplacer(`\n`, " ", `\"`, `"`, `\\`, `\`).Replace(m[2])
		if m[1] == "title" {
			titles = append(titles, v)
		} else if f[m[1]] == "" {
			f[m[1]] = v
		}
	}
	return f, titles
}

// given lists, in plain words, what the AI was shown, from the headings of its prompt.
func given(system, user string) []string {
	var out []string
	has := func(s string) bool { return strings.Contains(user, s) }
	if has("## Commit") {
		out = append(out, "the commit message")
	}
	if has("## Diff") {
		out = append(out, "the change (diff)")
	}
	if strings.Contains(user, "after the commit") {
		out = append(out, "the code after the change")
	}
	if strings.Contains(system, "team's rules") || strings.Contains(system, "This team's rules") {
		out = append(out, "your team's rules")
	}
	if has("## Already found by code checks") {
		out = append(out, "what the quick checks found")
	}
	if has("## What running this file's methods showed") {
		out = append(out, "what running the code showed")
	}
	if has("## Methods this change calls") || has("javadoc") {
		out = append(out, "the methods it calls")
	}
	if strings.Contains(system, "How this team judged earlier findings") {
		out = append(out, "findings you marked useful or wrong")
	}
	if has("## Definition of") {
		out = append(out, "the types it uses")
	}
	if has("## The disagreement") {
		out = append(out, "two opposite views to choose from")
	}
	return out
}

// choiceSide says which side a finished choice took: "claim" or "fine", from the prompt and the letter it picked.
func choiceSide(user, right string) string {
	aIsFine := strings.Contains(user, "Reviewer A says: The code after the commit is correct")
	if (right == "A") != aIsFine {
		return "claim"
	}
	return "fine"
}
