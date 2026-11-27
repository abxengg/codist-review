# codist-review

How [Codist](https://github.com/abxengg/codist) reviews a commit, and where it keeps the results.

- **Review** runs the exact code checks on every changed line, then asks the local model about the changed files.
  Each model claim must quote the code it means, as it is after the change or among the lines the change removed; a
  quote that is in neither is dropped, and a claim the code does not back (a null warning with nothing that makes the
  value null, say) is shown as a note rather than stated as fact. A removed `getOrDefault` or `orElse` counts as
  what makes a value null.
- **Exact facts before a second opinion:** a model claim the code contradicts becomes a note with the reason, before
  any second look: "strings compared with ==" on a line where Codist's type-aware check finds no string comparison,
  or "not implemented" when another file of the same commit does it. The model also sees the commit's other changes,
  so it stops calling things missing that the commit adds elsewhere. A serious claim that survives is checked once
  more by a second look.
- **Issues** are followed across commits by a key made of the rule or title, the file and the flagged line, so an
  issue is reported once and closed by the commit that fixes it. An issue whose first commit was amended, rebased or
  reset away can still be closed by a later fix, and an open issue moves with its file when the file is renamed.
  Catching up newest first, an issue found in an old commit that a later commit already fixed is closed at once.
- **Second look, a choice in both orders:** a serious model claim is put to the model again with the code before and
  after, the definitions of the types it names and the team's rules, as two reviewers: one with the claim, one saying
  the code is fine. The model picks the right one twice, with the two in swapped order, because models tend to pick
  by position. Picked both times, the claim stays an alarm; the code picked both times, or reasons about something
  else, make it a note with the reason. When the answers follow the order but both reasons are about the claim, it
  stays an alarm at medium for a person to judge. A failed call leaves the finding as it was. There is no yes-or-no
  question first: it said yes 45 times in 49, and its "could not tell" let claims through unchecked. On the three
  labs this took v0.12.0 from 54 to 60 of 62 planted bugs, with 1 of 14 clean commits alarmed instead of 2
  ([abxengg/codist#19](https://github.com/abxengg/codist/issues/19)).
- **Second reading:** `Options.Twice` reads each file again and keeps what only the second reading finds; Codist
  sets it for commits its risk model rates high, or for all with `twice = true`.
- **Reading plan:** above `MaxFiles` changed files, the model first says which files need reading and why; a person
  can ask for a skipped file (`Options.Force`).
- **Idle time:** `NextIdle` picks what an idle model lane reads: a recent commit through the specialist lenses
  (`Options.Deep`), then a commit older than the usual history (`NextOlder`).
- **Baseline** checks the whole code the first time Codist runs on a repository, crediting each issue to the commit
  that last changed its line.
- **Learning from feedback**: with `Learn` on, the model sees a few findings this team marked wrong or useful, as
  examples of what not to report and what is worth reporting here.
- **risk** learns from the repository's own history which commits tend to bring bugs (logistic regression, labels
  from fix commits traced back with git blame), tests itself on the newest commits, and is used only when it ranks
  clearly better than chance.
- **activity** shows what Codist is doing: each work lane's queue (running, waiting, finished), every model call with
  its prompt and reply, and the log as a live stream, for the interface's Activity page.
- **store** keeps reviews, findings, issues and incidents in one SQLite file, through a pure-Go driver.

```go
import (
    review "github.com/abxengg/codist-review"
    "github.com/abxengg/codist-review/store"
)

st, _ := store.Open(dataDir, "my-repo")
r := &review.Reviewer{Repo: repo, Store: st, Model: client, MaxFiles: 8}
err := r.Review(ctx, sha, review.Options{})
```

```bash
go test -tags 'grammar_subset grammar_subset_java' ./...
```

## Adding a language

A review needs five things from a language, written as the `Language` interface in `lang.go`: which files it owns,
which are tests, how to parse a file into lines and units (classes and their methods), what to learn from the whole
project once per HEAD (`Facts`), and its checks. Java comes from codist-java (`review.Java`).

To add one, say TypeScript:

1. Make a module such as `codist-ts` that parses and checks TypeScript. tree-sitter has a TypeScript grammar, so
   the parser Codist already uses can read it.
2. In it, or in this repository, write a small type with the five methods, the way `javaLang` in `lang.go` wraps
   codist-java. `lang_test.go` has a made-up language in 20 lines to start from.
3. List it: `Reviewer{Languages: []review.Language{review.Java, ts.Language}}`.

Everything else works for the new language as it is: findings and issues, following an issue until a commit fixes
it, the model's review with its claim checks, and Useful/Wrong learning. Still Java only: the callers and contracts
shown to the model, the code graph, the Repository page and running changed methods before and after a commit. Each
of those needs its own part of the language later.

## Licence

Source available under the [PolyForm Noncommercial License 1.0.0](LICENSE): free for any noncommercial purpose, such
as personal study, research, hobby projects and testing, and for use by charities, schools, public research and
public health bodies and government institutions. **Using it for a business, or in anything sold, needs a commercial
licence** from the author: open an issue at [github.com/abxengg/codist](https://github.com/abxengg/codist/issues)
to ask. Third-party code included in Codist keeps its own licence.

Required Notice: Copyright 2026 Abinash (https://github.com/AbiXnash)
