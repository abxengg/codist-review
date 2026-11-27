package review

import (
	"strings"
	"testing"

	"github.com/abxengg/codist-git"
)

const directoryAfter = `package com.recordslab.service;

import java.util.ArrayList;
import java.util.List;
import org.springframework.stereotype.Service;

@Service
public class Directory {
    private final EmployeeRepository employees;
    private final DepartmentRepository departments;

    public Directory(EmployeeRepository employees, DepartmentRepository departments) {
        this.employees = employees;
        this.departments = departments;
    }

    /** Everyone, for a full export. */
    public List<PublicEmployee> everyone() {
        List<PublicEmployee> out = new ArrayList<>();
        for (Employee e : employees.all()) {
            Department d = departments.find(e.departmentId()).orElseThrow();
            out.add(PublicEmployee.of(e, d));
        }
        return out;
    }
}
`

// records-lab "Drop the transaction around exports, they only read": the commit removes @Transactional(readOnly = true)
// from a method that still reads employees and departments, so the export no longer sees one snapshot.
const directoryPatch = `@@ -14,7 +14,6 @@ import java.util.List;
 import org.springframework.stereotype.Service;
-import org.springframework.transaction.annotation.Transactional;
 
 @Service
@@ -17,8 +16,7 @@ public class Directory {
     }
 
-    /** Everyone, for a full export: one snapshot (rule 11). */
-    @Transactional(readOnly = true)
+    /** Everyone, for a full export. */
     public List<PublicEmployee> everyone() {
         List<PublicEmployee> out = new ArrayList<>();
         for (Employee e : employees.all()) {`

func TestRemovedTransaction(t *testing.T) {
	const path = "src/main/java/com/recordslab/service/Directory.java"
	r := &Reviewer{}
	check := func(src, patch string) []string {
		var got []string
		for _, f := range r.removedTransactions("abc", git.FileChange{Path: path, Patch: patch}, Java.Parse(path, []byte(src))) {
			got = append(got, f.Rule+" "+f.Title)
		}
		return got
	}
	got := check(directoryAfter, directoryPatch)
	if len(got) != 1 || !strings.Contains(got[0], "everyone() still reads the database 2 times") {
		t.Fatalf("records-lab case: %q", got)
	}
	// Moved to the class: still transactional, nothing to report.
	if got := check(strings.Replace(directoryAfter, "@Service\n", "@Service\n@Transactional(readOnly = true)\n", 1), directoryPatch); len(got) != 0 {
		t.Errorf("class-level transaction: %q", got)
	}
	// A method that reads once loses little.
	once := strings.Replace(directoryAfter, "            Department d = departments.find(e.departmentId()).orElseThrow();\n", "            Department d = null;\n", 1)
	if got := check(once, directoryPatch); len(got) != 0 {
		t.Errorf("one read: %q", got)
	}
	// No @Transactional removed (only the import): nothing.
	importOnly := "@@ -14,7 +14,6 @@ import java.util.List;\n import org.springframework.stereotype.Service;\n-import org.springframework.transaction.annotation.Transactional;\n \n @Service"
	if got := check(directoryAfter, importOnly); len(got) != 0 {
		t.Errorf("import only: %q", got)
	}
}
