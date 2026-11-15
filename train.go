package review

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	git "github.com/abxengg/codist-git"
	"github.com/abxengg/codist-review/store"
)

// What became of the model's findings is training data that never leaves the machine: a person marked one wrong, or
// a later commit changed the line it flagged. Research on review bots finds a small model with an adapter trained on
// such outcomes tells wanted findings from unwanted ones better than a prompt does. Codist does not train: it writes
// the examples in the chat format the usual tools read (MLX, Unsloth, llama.cpp finetune), and the adapter they make
// can be put on the Ollama model with ADAPTER in a Modelfile.

const trainSystem = `You are shown a finding that a code reviewer raised on a commit, with the code it is about. ` +
	`Answer "useful" if the team would act on it, or "wrong" if they would dismiss it. Answer with that one word.`

// TrainingSet writes one JSON line for each finding of the model whose outcome is known, and returns how many were
// useful and how many wrong. A finding counts as useful when a person said so or the flagged line was later fixed.
func (r *Reviewer) TrainingSet(w io.Writer) (useful, wrong int, err error) {
	feedback := r.Store.AllFeedback()
	enc := json.NewEncoder(w)
	for _, is := range r.Store.AllIssues() {
		f := is.Latest
		if f.Source != "model" {
			continue
		}
		label := ""
		switch v := feedback[is.Key].Verdict; {
		case v == "wrong":
			label = "wrong"
		case v == "useful" || is.State == "resolved":
			label = "useful"
		default:
			continue
		}
		prompt := r.trainPrompt(f)
		if prompt == "" {
			continue // its commit is gone from the repository
		}
		if err := enc.Encode(map[string]any{"messages": []map[string]string{
			{"role": "system", "content": trainSystem}, {"role": "user", "content": prompt}, {"role": "assistant", "content": label}}}); err != nil {
			return useful, wrong, err
		}
		if label == "useful" {
			useful++
		} else {
			wrong++
		}
	}
	return useful, wrong, nil
}

// trainPrompt shows a finding as the model first made it: the claim, the commit, and the code around the line.
func (r *Reviewer) trainPrompt(f store.Finding) string {
	c, err := r.Repo.Get(f.SHA)
	if err != nil {
		return ""
	}
	src, ok := r.Repo.FileAt(c.SHA, f.Path)
	if !ok {
		return ""
	}
	lines := strings.Split(string(src), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "## The finding\nFile %s, line %d, severity %s.\n%s\n%s\n\n## Commit\n%s\n\n## Code\n```\n", f.Path, f.Line, f.Severity, f.Title, f.Detail, c.Subject)
	for l := max(1, f.Line-15); l <= min(len(lines), max(f.Line, f.EndLine)+15); l++ {
		fmt.Fprintf(&b, "%4d| %s\n", l, lines[l-1])
	}
	b.WriteString("```\n")
	return b.String()
}

// ExportTraining writes the training set of the repository's store in dataDir to w, and returns a line for the
// person saying how much there is and what to do with it.
func ExportTraining(dataDir string, repo *git.Repo, w io.Writer) (string, error) {
	st, err := store.OpenRepo(dataDir, repo.Root)
	if err != nil {
		return "", err
	}
	defer st.Close()
	useful, wrong, err := (&Reviewer{Repo: repo, Store: st}).TrainingSet(w)
	note := "Enough to try training an adapter."
	if useful+wrong < 200 {
		note = "That is few: an adapter wants some hundreds. Keep marking findings useful or wrong."
	}
	return fmt.Sprintf("%d useful and %d wrong findings written, one JSON line each. %s", useful, wrong, note), err
}
