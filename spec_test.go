package review

import (
	"context"
	"os"
	"strings"
	"testing"

	git "github.com/abxengg/codist-git"
)

func TestNumberChanges(t *testing.T) {
	patch := "--- a/A.java\n+++ b/A.java\n@@ -3,4 +3,4 @@ class A {\n     int a;\n-    static final double DIM_EVERY = 20.0;\n+    static final double DIM_EVERY = 10.0;\n" +
		"-    static final double STEP = 1.0 / 60.0;\n+    static final double STEP = 1.0 / 30.0;\n@@ -20,3 +20,3 @@\n-    // waits 5 seconds\n+    // waits 6 seconds\n" +
		"-    int x = pick(2, 3);\n+    int x = pick(4, 5);\n-    long big = 10_000L;\n+    long big = 10000;\n"
	got := numberChanges(patch)
	if len(got) != 2 || got[0] != (numberChange{4, "20", "10"}) || got[1] != (numberChange{5, "60", "30"}) {
		t.Errorf("a comment, two numbers at once and the same number written another way are not changes: %+v", got)
	}
}

// A commit that changes a number the spec states is reported without a model; one that changes the spec too, or a
// number no document speaks of, is not. The issue closes when the line goes back.
func TestSpecNumbers(t *testing.T) {
	tr, r, st := setup(t)
	lantern := func(every, radius string) string {
		return "package com.shop.order;\n\npublic class Lantern {\n    public static final double DIM_EVERY = " + every +
			";\n    public static final int RADIUS = " + radius + ";\n    static final int RETRIES = 7;\n}\n"
	}
	const path = "order-service/src/main/java/com/shop/order/Lantern.java"
	spec := "# Spec\n\n9. **The lantern** lights 5 tiles around you, one tile less every 20 seconds,\n   never fewer than 2.\n\n" +
		"7. You start with 3 lives and gain one each 10,000 points.\n\n```\nRETRIES 7 lantern\n```\n"
	tr.Commit("Add the lantern", map[string]string{path: lantern("20.0", "5"), "docs/SPEC.md": spec})
	tr.Commit("Dim the lantern faster", map[string]string{path: lantern("10.0", "5")})
	bad := tr.Head()
	if err := r.Review(context.Background(), bad, Options{RulesOnly: true}); err != nil {
		t.Fatal(err)
	}
	fs := st.Findings(bad)
	if len(fs) != 1 || fs[0].Rule != specRule || fs[0].Line != 4 || !strings.Contains(fs[0].Title, "Changes 20 to 10, but docs/SPEC.md says 20") ||
		!strings.Contains(fs[0].Detail, "docs/SPEC.md, line 3") || fs[0].State != "open" {
		t.Fatalf("%+v", fs)
	}
	// Another commit to the same file leaves the issue open: no whole-file check reports it, its line does.
	tr.Commit("More retries", map[string]string{path: strings.Replace(lantern("10.0", "5"), "RETRIES = 7", "RETRIES = 9", 1)})
	if err := r.Review(context.Background(), tr.Head(), Options{RulesOnly: true}); err != nil {
		t.Fatal(err)
	}
	if fs := st.Findings(tr.Head()); len(fs) != 0 {
		t.Errorf("a number only a code block mentions: %+v", fs)
	}
	if st.Findings(bad)[0].State != "open" {
		t.Error("the issue was closed by a commit that did not touch its line")
	}
	// Changing the document with the code is a decision, not a slip.
	tr.Commit("Light 6 tiles", map[string]string{path: strings.Replace(lantern("10.0", "6"), "RETRIES = 7", "RETRIES = 9", 1),
		"docs/SPEC.md": strings.Replace(spec, "lights 5 tiles", "lights 6 tiles", 1)})
	if err := r.Review(context.Background(), tr.Head(), Options{RulesOnly: true}); err != nil {
		t.Fatal(err)
	}
	if fs := st.Findings(tr.Head()); len(fs) != 0 {
		t.Errorf("the document changed too: %+v", fs)
	}
	tr.Commit("Dim as the spec says", map[string]string{path: strings.Replace(lantern("20.0", "6"), "RETRIES = 7", "RETRIES = 9", 1)})
	if err := r.Review(context.Background(), tr.Head(), Options{RulesOnly: true}); err != nil {
		t.Fatal(err)
	}
	if fs := st.Findings(bad); fs[0].State != "resolved" {
		t.Errorf("putting the value back closes the issue: %+v", fs[0])
	}
}

// CODIST_SPEC_REPO=<path> lists what the check says about every commit of a real repository.
func TestSpecNumbersOnARepository(t *testing.T) {
	root := os.Getenv("CODIST_SPEC_REPO")
	if root == "" {
		t.Skip("set CODIST_SPEC_REPO to a repository")
	}
	repo, err := git.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	r := &Reviewer{Repo: repo}
	log, _ := repo.Log(5000)
	n := 0
	for _, c := range log {
		changes, _ := repo.Changes(c.SHA)
		r.docs = nil
		for _, ch := range changes {
			lang := r.langOf(ch.Path)
			if lang == nil || ch.Status == "deleted" || lang.IsTest(ch.Path) {
				continue
			}
			src, ok := repo.FileAt(c.SHA, ch.Path)
			if !ok {
				continue
			}
			for _, f := range r.specNumbers(c.SHA, ch, lang.Parse(ch.Path, src)) {
				n++
				t.Logf("%s %s\n   %s:%d %s\n   %s", c.Short, c.Subject, f.Path, f.Line, f.Title, f.Detail)
			}
		}
	}
	t.Logf("%d commits, %d findings", len(log), n)
}
