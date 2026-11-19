package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abxengg/codist-git"
	"github.com/abxengg/codist-git/testrepo"
	"github.com/abxengg/codist-review/store"
)

func TestJudge(t *testing.T) {
	code := strings.Split(`class A {
  String a(Map<String, B> m, String k) {
    B b = m.get(k);
    return b.name();
  }
  String c(C c) {
    return c.name();
  }
  String d(D d) {
    if (d == null) { log(); }
    return d.name();
  }
  int e(List<Item> items) {
    return items.stream()
        .mapToInt(Item::price)
        .sum();
  }
  long f(long id) {
    Order o = repo.findById(id).orElseThrow();
    return o.total();
  }
  void g(String role) {
    if (!HR.equals(role)) { deny(); }
  }
  Map<String, Integer> h() {
    return new LinkedHashMap<String, Integer>(this.headerMap);
  }
  boolean i(Object o) {
    return this.name.equals(o);
  }
}`, "\n")
	cases := []struct {
		name, severity, title, evidence, patch string
		line                                   int
		drop                                   bool
		want                                   string
		doubted                                bool
	}{
		{"value from a map lookup", "high", "Null dereference", "return b.name();", "", 4, false, "high", false},
		{"value tested for null nearby", "high", "Possible NullPointerException", "return d.name();", "", 11, false, "high", false},
		{"nothing makes it null", "high", "Null dereference on some path", "return c.name();", "", 7, false, "low", true},
		{"the change removed a null check", "high", "Null header not handled", "return c.name();", "-  if (c == null) return;\n", 7, false, "high", false},
		{"only describes the change", "medium", "Improved variable naming", "return c.name();", "", 7, false, "low", true},
		{"an ordinary claim is untouched", "high", "Wrong total for empty orders", "return c.name();", "", 7, false, "high", false},
		{"a quote joined from several lines", "medium", "Sum ignores discounts", "return items.stream().mapToInt(Item::price).sum();", "", 14, false, "medium", false},
		{"no quote at all", "high", "Wrong total", "", "", 7, false, "low", true},
		{"quote not in the file", "high", "Wrong total", "return x.total();", "", 7, true, "", false},
		{"quote far away", "high", "Wrong total", "class A {", "", 16, true, "", false},
		{"vague advice", "medium", "Consider adding validation", "return c.name();", "", 7, true, "", false},
		{"orElseThrow never gives null", "high", "NullPointerException risk", "return o.total();", "", 20, false, "low", true},
		{"only says the message disagrees", "high", "Commit message contradicts code change", "return c.name();", "", 7, false, "low", true},
		{"line outside the file", "high", "Wrong total", "return c.name();", "", 99, true, "", false},
		{"null-safe equals, though a null check was removed", "high", "Null Pointer Exception", "if (!HR.equals(role)) { deny(); }", "-  if (role != null && role != HR) {\n", 23, false, "low", true},
		{"a mismatch that is not about the message", "high", "Path separator mismatch on Windows causes file lookup failure", "return c.name();", "", 7, false, "high", false},
		{"SQL's NULL is not a null claim", "high", "MySQL format no longer uses 'N' as the NULL string", "return c.name();", "", 7, false, "high", false},
		{"returns null instead of a value is a wrong value, not a crash", "high", "isA matcher returns null instead of an instance of the class", "return c.name();", "", 7, false, "high", false},
		{"a crash on null still needs a source", "high", "NullPointerException when c is null", "return c.name();", "", 7, false, "low", true},
		{"a field read through this, whose null test the change removed", "high", "NullPointerException when headerMap is null", "return new LinkedHashMap<String, Integer>(this.headerMap);", "-        return this.headerMap == null ? null : new LinkedHashMap<String, Integer>(this.headerMap);\n", 26, false, "high", false},
		{"a field read through this, nothing makes it null", "high", "NullPointerException when headerMap is null", "return new LinkedHashMap<String, Integer>(this.headerMap);", "", 26, false, "low", true},
		{"a field called through this, its null test removed", "high", "NullPointerException on name", "return this.name.equals(o);", "-    return this.name != null && this.name.equals(o);\n", 29, false, "high", false},
	}
	for _, c := range cases {
		drop, sev, doubt := judge(code, c.patch, c.line, c.severity, c.title, c.evidence)
		if (drop != "") != c.drop || (!c.drop && (sev != c.want || (doubt != "") != c.doubted)) {
			t.Errorf("%s: drop %q, severity %q, doubt %q", c.name, drop, sev, doubt)
		}
	}
}

// A commit adds a swallowed exception; a later commit removes it, which resolves the issue.
func TestReviewAndResolve(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	path := filepath.Join(dir, "svc", "src", "main", "java", "A.java")
	os.MkdirAll(filepath.Dir(path), 0o755)
	write := func(s string) { os.WriteFile(path, []byte(s), 0o644) }
	run("init", "-q")
	write("class A {\n  void a() {\n    run();\n  }\n}\n")
	run("add", ".")
	run("commit", "-q", "-m", "Add A")
	write("class A {\n  void a() {\n    try {\n      run();\n    } catch (Exception e) {\n    }\n  }\n}\n")
	run("commit", "-q", "-am", "Guard run")
	bad := head(t, dir)

	repo, _ := git.Open(dir)

	t.Cleanup(repo.Close) // stop its git process before the folder is removed
	st, err := store.Open(t.TempDir(), "r")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Reviewer{Repo: repo, Store: st, MaxFiles: 8}
	if err := r.Review(context.Background(), bad[:7], Options{}); err != nil { // a short id is stored under the full one
		t.Fatal(err)
	}
	fs := st.Findings(bad)
	if len(fs) != 1 || fs[0].Rule != "empty-catch" || fs[0].Line != 5 || fs[0].State != "open" {
		t.Fatalf("findings: %+v", fs)
	}
	if rv, _ := st.GetReview(bad); rv.Status != "done" {
		t.Errorf("status %q", rv.Status)
	}
	write("class A {\n  void a() {\n    try {\n      run();\n    } catch (Exception e) {\n      throw new IllegalStateException(e);\n    }\n  }\n}\n")
	run("commit", "-q", "-am", "Rethrow")
	fix := head(t, dir)
	if err := r.Review(context.Background(), fix, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := st.Findings(bad); got[0].State != "resolved" || len(st.Findings(fix)) != 0 {
		t.Errorf("after the fix: %+v, fix findings %+v", got, st.Findings(fix))
	}
}

// Catching up on history, Codist reads the newest commits first, so the fix is reviewed before the bug it fixes. The
// bug's issue must still close, credited to the fix, while an issue no later commit fixed stays open.
func TestNewestFirstStillResolves(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	a := filepath.Join(dir, "svc", "src", "main", "java", "A.java")
	b := filepath.Join(dir, "svc", "src", "main", "java", "B.java")
	os.MkdirAll(filepath.Dir(a), 0o755)
	run("init", "-q")
	os.WriteFile(a, []byte("class A {\n  void a() {\n    run();\n  }\n}\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "Add A")
	os.WriteFile(a, []byte("class A {\n  void a() {\n    try {\n      run();\n    } catch (Exception e) {\n    }\n  }\n}\n"), 0o644)
	os.WriteFile(b, []byte("class B {\n  void b() {\n    try {\n      run();\n    } catch (Exception e) {\n    }\n  }\n}\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "Guard run")
	bad := head(t, dir)
	os.WriteFile(a, []byte("class A {\n  void a() {\n    run();\n  }\n}\n"), 0o644)
	run("commit", "-q", "-am", "Let errors through in A")
	fix := head(t, dir)

	repo, _ := git.Open(dir)

	t.Cleanup(repo.Close) // stop its git process before the folder is removed
	st, err := store.Open(t.TempDir(), "r")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Reviewer{Repo: repo, Store: st, MaxFiles: 8}
	for _, sha := range []string{fix, bad} { // newest first, as the backlog runs
		if err := r.Review(context.Background(), sha, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	state := map[string]string{}
	for _, f := range st.Findings(bad) {
		state[filepath.Base(f.Path)] = f.State
	}
	if state["A.java"] != "resolved" || state["B.java"] != "open" {
		t.Errorf("want A's issue resolved by the later fix and B's still open, got %v", state)
	}
}

func head(t *testing.T, dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestWithDeletions(t *testing.T) {
	patch := "@@ -20,4 +20,3 @@ class A {\n     Order one(long c, long id) {\n         return orders.findById(id)\n-            .filter(o -> o.customerId() == c)\n             .orElseThrow();\n"
	got := withDeletions(nil, patch)
	if len(got) != 2 || got[0] != 21 || got[1] != 22 {
		t.Fatalf("lines around the deletion: %v", got)
	}
	if got := withDeletions([]int{5}, "@@ -5,1 +5,1 @@\n-a\n+b\n"); len(got) != 1 || got[0] != 5 {
		t.Fatalf("a replaced line: %v", got)
	}
}

// A merge is reviewed on what was written by hand to settle it: here the person merging left an empty catch.
func TestMergeIsReviewedOnItsHandWrittenPart(t *testing.T) {
	tr, r, st := setup(t)
	const p = "order-service/src/main/java/com/shop/order/Extra.java"
	body := func(s string) string {
		return "package com.shop.order;\n\npublic class Extra {\n    int f(int n) {\n" + s + "    }\n}\n"
	}
	tr.Commit("Add Extra", map[string]string{p: body("        return n;\n")})
	tr.Git("checkout", "-q", "-b", "side")
	tr.Commit("Side", map[string]string{p: body("        return n + 1;\n")})
	tr.Git("checkout", "-q", "main")
	tr.Commit("Main", map[string]string{p: body("        return n + 2;\n")})
	if out, err := exec.Command("git", "-C", tr.Dir, "merge", "side").CombinedOutput(); err == nil {
		t.Fatalf("expected a conflict: %s", out)
	}
	tr.Commit("Merge side", map[string]string{p: body("        try {\n            return n + 3;\n        } catch (Exception e) {\n        }\n        return 0;\n")})
	if err := r.Review(context.Background(), tr.Head(), Options{RulesOnly: true}); err != nil {
		t.Fatal(err)
	}
	fs := st.Findings(tr.Head())
	if len(fs) != 1 || fs[0].Rule != "empty-catch" {
		t.Errorf("%+v", fs)
	}
}

// A quote with single quotes matches code with double quotes: models are told not to type a double quote.
func TestQuotesMatchWhateverTheQuotationMark(t *testing.T) {
	lines := []string{"class A {", `    if (key.equals("P")) {`, "    }", "}"}
	if drop, _, _ := judge(lines, "", 2, "high", "Pause never works", "if (key.equals('P')) {"); drop != "" {
		t.Errorf("a quote with single quotes was dropped: %s", drop)
	}
	if got := verdictOf("confirmed", "key.equals('P')", "", "It compares by value.", `if (key.equals("P")) {`); got != "confirmed" {
		t.Errorf("second look quote: %s", got)
	}
	// A quote that leaves the marks out altogether matches too.
	if got := verdictOf("confirmed", "private static final String FORMULA_STARTS = =+@;", "", "The hyphen was removed.",
		`    private static final String FORMULA_STARTS = "=+@";`); got != "confirmed" {
		t.Errorf("a quote without its marks: %s", got)
	}
}

// A commit that brought an issue is amended, so its id changes; a later commit fixes the issue. The issue is closed
// although the commit it was first seen in is in no branch any more.
func TestFixAfterRewrittenCommit(t *testing.T) {
	tr, r, st := setup(t)
	ctx := context.Background()
	tr.Git("reset", "-q", "--hard", tr.Bug)
	if err := r.Review(ctx, tr.Bug, Options{}); err != nil || len(st.OpenIssues(testrepo.ServicePath)) != 1 {
		t.Fatalf("the bug opens one issue: %v %d", err, len(st.OpenIssues(testrepo.ServicePath)))
	}
	tr.Git("commit", "-q", "--amend", "-m", "Add discount, reworded")
	amended := tr.Head()
	if err := r.Review(ctx, amended, Options{}); err != nil {
		t.Fatal(err)
	}
	if open := st.OpenIssues(testrepo.ServicePath); len(open) != 1 || open[0].FirstSHA != amended {
		t.Errorf("the issue follows the commit that replaced its first one: %+v", open)
	}
	tr.Commit("Rethrow discount errors", map[string]string{testrepo.ServicePath: testrepo.ServiceFix})
	if err := r.Review(ctx, tr.Head(), Options{}); err != nil || len(st.OpenIssues(testrepo.ServicePath)) != 0 {
		t.Errorf("the fix closes it: %v %+v", err, st.OpenIssues(testrepo.ServicePath))
	}
}

// The commits that brought an issue are thrown away with a reset and never reviewed again; the next commit that
// changes the file closes the issue, which is no longer in the code.
func TestFixAfterReset(t *testing.T) {
	tr, r, st := setup(t)
	ctx := context.Background()
	tr.Git("reset", "-q", "--hard", tr.Bug)
	if err := r.Review(ctx, tr.Bug, Options{}); err != nil || len(st.OpenIssues(testrepo.ServicePath)) != 1 {
		t.Fatalf("the bug opens one issue: %v", err)
	}
	tr.Git("reset", "-q", "--hard", tr.Init)
	tr.Git("branch", "-q", "-D", testrepo.Branch) // the other branch starts after the bug, so it would keep it alive
	tr.Commit("Rethrow discount errors", map[string]string{testrepo.ServicePath: testrepo.ServiceFix})
	if err := r.Review(ctx, tr.Head(), Options{}); err != nil || len(st.OpenIssues(testrepo.ServicePath)) != 0 {
		t.Errorf("an issue of a commit that is gone is closed: %v %+v", err, st.OpenIssues(testrepo.ServicePath))
	}
}

// A file with an open issue is renamed: the issue moves to the new name, and a fix there closes it.
func TestIssueFollowsRenamedFile(t *testing.T) {
	tr, r, st := setup(t)
	ctx := context.Background()
	tr.Git("reset", "-q", "--hard", tr.Bug)
	if err := r.Review(ctx, tr.Bug, Options{}); err != nil || len(st.OpenIssues(testrepo.ServicePath)) != 1 {
		t.Fatalf("the bug opens one issue: %v", err)
	}
	moved := strings.Replace(testrepo.ServicePath, "OrderService", "Orders", 1)
	tr.Git("mv", testrepo.ServicePath, moved)
	tr.Git("commit", "-q", "-m", "Rename the order service file")
	if err := r.Review(ctx, tr.Head(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(st.OpenIssues(testrepo.ServicePath)) != 0 || len(st.OpenIssues(moved)) != 1 {
		t.Fatalf("the issue moves with the file: %d under the old name, %d under the new", len(st.OpenIssues(testrepo.ServicePath)), len(st.OpenIssues(moved)))
	}
	if list := st.OpenIssueList(); len(list) != 1 || list[0].Path != moved {
		t.Errorf("the list of open issues shows it under the new name: %+v", list)
	}
	tr.Commit("Rethrow discount errors", map[string]string{moved: testrepo.ServiceFix})
	if err := r.Review(ctx, tr.Head(), Options{}); err != nil || len(st.OpenIssues(moved)) != 0 {
		t.Errorf("the fix in the renamed file closes it: %v %+v", err, st.OpenIssues(moved))
	}
}

// Catching up newest first, the commit that renamed a file is reviewed before the older commit that brought an issue
// into it. The issue is then followed to the new name: open while it is still there, closed by the commit that fixed it.
func TestOlderIssueFollowsLaterRename(t *testing.T) {
	tr, r, st := setup(t)
	ctx := context.Background()
	tr.Git("reset", "-q", "--hard", tr.Bug)
	moved := strings.Replace(testrepo.ServicePath, "OrderService", "Orders", 1)
	tr.Git("mv", testrepo.ServicePath, moved)
	tr.Git("commit", "-q", "-m", "Rename the order service file")
	rename := tr.Head()
	for _, sha := range []string{rename, tr.Bug} { // newest first
		if err := r.Review(ctx, sha, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.OpenIssues(testrepo.ServicePath)) != 0 || len(st.OpenIssues(moved)) != 1 {
		t.Fatalf("still in the renamed file: %d under the old name, %d under the new", len(st.OpenIssues(testrepo.ServicePath)), len(st.OpenIssues(moved)))
	}

	tr2, r2, st2 := setup(t)
	tr2.Git("reset", "-q", "--hard", tr2.Bug)
	tr2.Git("mv", testrepo.ServicePath, moved)
	tr2.Git("commit", "-q", "-m", "Rename the order service file")
	rename = tr2.Head()
	tr2.Commit("Rethrow discount errors", map[string]string{moved: testrepo.ServiceFix})
	for _, sha := range []string{tr2.Head(), rename, tr2.Bug} {
		if err := r2.Review(ctx, sha, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(st2.OpenIssueList()); n != 0 {
		t.Errorf("fixed after the rename, so closed: %d open", n)
	}
	for _, is := range st2.AllIssues() {
		if is.State != "resolved" || is.FixedSHA != tr2.Head() {
			t.Errorf("closed by the commit that fixed it: %+v", is.State+" "+is.FixedSHA)
		}
	}
}
