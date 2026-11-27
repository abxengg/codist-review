package review

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/abxengg/codist-git"
	"github.com/abxengg/codist-review/store"
)

// A transaction taken away. A commit that removes @Transactional from a method that still reads the database more
// than once leaves each read to see the database at its own moment, so one result can mix data from before and
// after a change made meanwhile. The model missed this on records-lab ("Drop the transaction around exports, they
// only read"): the change is a deletion, and the new code alone looks fine. Only the diff shows it, so this check
// reads the removed lines. A method that never had the annotation is not reported: many read twice on purpose.

const txRule = "transaction-removed"

var (
	removedTx = regexp.MustCompile(`^-\s*@(org\.springframework\.transaction\.annotation\.)?Transactional\b`)
	// A field whose type reads from a database: a Spring Data repository, a DAO, JDBC or JPA.
	dataType = regexp.MustCompile(`(Repository|Repo|Dao|DAO|JdbcTemplate|JdbcClient|EntityManager|MongoTemplate|JdbcOperations)$`)
)

// removedTransactions reports a method of one changed Java file that lost its @Transactional in this commit while
// it still makes two or more calls to the database.
func (r *Reviewer) removedTransactions(sha string, ch git.FileChange, f Parsed) []store.Finding {
	j, ok := f.(javaFile)
	if !ok || !strings.Contains(ch.Patch, "Transactional") {
		return nil
	}
	var out []store.Finding
	for _, at := range removedAnnotationLines(ch.Patch) {
		for _, t := range j.f.Types {
			if slices.ContainsFunc(t.Annotations, isTransactional) {
				continue // the class is still transactional, so its methods are
			}
			fields := map[string]string{}
			for _, fl := range t.Fields {
				fields[fl.Name] = fl.Type
			}
			for _, m := range t.Methods {
				// The annotation sat just above the method: after the removal the method starts at or right after it.
				if m.Line < at || m.Line > at+3 || slices.ContainsFunc(m.Annotations, isTransactional) {
					continue
				}
				var reads []string
				for _, c := range m.Calls {
					recv := strings.TrimPrefix(c.Receiver, "this.")
					if typ := fields[recv]; typ != "" && dataType.MatchString(simpleName(typ)) {
						reads = append(reads, recv+"."+c.Name+"()")
					}
				}
				if len(reads) < 2 {
					continue
				}
				shown := reads
				if len(shown) > 4 {
					shown = append(shown[:4:4], "…")
				}
				out = append(out, finding(sha, ch.Path, f, m.Line, m.Line, "medium", "rule", txRule,
					fmt.Sprintf("@Transactional removed, but %s() still reads the database %d times", m.Name, len(reads)),
					fmt.Sprintf("This commit takes the transaction away from %s(), which still calls %s. Without it each call sees the database at its own moment, so one result can mix data from before and after a change made in between, and a lazy relation read outside the transaction can fail.",
						m.Name, strings.Join(shown, ", ")),
					"Keep @Transactional(readOnly = true) on the method, or say in the commit why the reads may now disagree.", ""))
			}
		}
	}
	return out
}

func isTransactional(a string) bool {
	return strings.HasPrefix(strings.TrimPrefix(strings.TrimPrefix(a, "@"), "org.springframework.transaction.annotation."), "Transactional")
}

func simpleName(t string) string {
	if i := strings.IndexAny(t, "<["); i >= 0 {
		t = t[:i]
	}
	return t[strings.LastIndex(t, ".")+1:]
}

// removedAnnotationLines are the places in the new file, as line numbers, where the patch removed an @Transactional.
func removedAnnotationLines(patch string) []int {
	var out []int
	n := 0
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "@@"):
			if m := hunkNew.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "---"), strings.HasPrefix(l, "+++"):
		case strings.HasPrefix(l, "-"):
			if removedTx.MatchString(l) {
				out = append(out, n)
			}
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, " "):
			n++
		}
	}
	return out
}
