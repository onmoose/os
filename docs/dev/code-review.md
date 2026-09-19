# Code review

Source of truth for reviewing a PR on moose. Used both by contributors running a self-review on their own open PR (Step 7 of [`contributing.md`](contributing.md)) and by a dedicated review agent. The lenses here define what "reviewed" means on this project.

## Before you start

Read these before looking at a single line of diff — familiarity with the spec is what separates a useful review from a superficial one:

1. **The issue being closed** — understand what was asked, not just what was built.
2. **`docs/progress/<slug>.md`** — the contributor's account of what was done, what was skipped, and what's next.
3. **The spec section(s) named in the progress entry** — the binding contract for the behavior. Read the full section, not just a keyword search.
4. **`CLAUDE.md`** — the coding conventions and load-bearing decisions every PR is held to.

When reviewing an already-open PR, also read the existing review comments first — see [Prior review comments](#prior-review-comments).

## Prior review comments

**Every review of an open PR checks the automated reviewers, every time.** Greptile runs on this repo and posts its review as a PR comment. It is slower than a review agent, so on a PR opened moments ago its comment is usually **not there yet** — an empty comment list means "not posted yet", never "nothing found". Check for it before you call the review finished; if it has not landed, wait and check again rather than reporting a clean review it might contradict. A self-review that skipped Greptile is not a finished self-review. This is not hypothetical: on #435 the agent review returned no findings and Greptile then caught a real bug in the same diff.

When the PR carries review comments — from a human or from an AI code reviewer (Greptile, Copilot, CodeRabbit, and similar) — read them before writing your own findings. Read both the conversation thread *and* the inline review comments left on specific lines. Fetch them with `gh pr view <N> --comments` for the thread and `gh api repos/{owner}/{repo}/pulls/<N>/comments` for the line-level review comments.

For each prior comment, do one of three things:

- **Confirm and reinforce** — the issue is real and still present in the latest diff. Cite that a reviewer already flagged it, and raise the severity if it has been ignored across commits: an unaddressed repeat finding is a stronger signal than a fresh one.
- **Dismiss** — it's a false positive or was already addressed in a later commit. Say so in one line with the reason, so the maintainer doesn't have to re-adjudicate it.
- **Extend** — it points at a real problem but missed the full scope. Take it further and cover what it left out.

Never silently re-state a finding another reviewer already made, and never silently drop one. AI-reviewer comments are input, not authority: verify each against the actual diff and the spec rather than deferring to it.

## Severity levels

**Block** — the PR must not merge as-is. A correctness bug, security issue, spec deviation, or convention violation that will cause real harm or future debt. Always include a fix or a path to one.

**Note** — worth calling out but not a blocker. A better approach, a missing test for a low-probability path, a style issue. The contributor can address it now or file a follow-up issue.

**Question** — you are not certain whether something is a problem. State the uncertainty clearly; don't guess or inflate to Block. The contributor or maintainer decides.

When in doubt, escalate to Block rather than downgrade to Note. A false Block costs one round-trip; a missed bug costs much more.

## Review lenses

### 1. Correctness

- Logic errors: wrong conditions, inverted booleans, off-by-one in slices or pagination cursors.
- Error paths: every `if err != nil` branch must return or handle — errors silently dropped with `_ = err` or a missing check are almost always bugs.
- Nil/empty handling: functions receiving a pointer, slice, or map that don't guard against nil/empty before use.
- Concurrency: shared state accessed without a lock; goroutines that outlive their context; channels never closed.
- SQL: argument order vs. placeholder order; missing `WHERE` on mutations; unbounded queries with no `LIMIT`.

### 2. Security

- Auth gates: every handler returning user data or mutating state must verify caller identity and role. Check that the gate is applied, not just that it exists somewhere in the file.
- Input validation: external input (request body fields, URL params, query strings) must be validated at the handler boundary before reaching store or business logic.
- SQL injection: only parameterized queries — no string interpolation into query text.
- Privilege escalation: operations that change roles, passwords, or permissions must enforce the caller's own privilege level.
- Sensitive data in logs: no passwords, tokens, or secrets in `slog` calls.

### 3. Spec fidelity

- Does the implementation match the spec section(s) named in the progress entry, including edge cases the spec calls out explicitly?
- If the implementation diverges, is it documented in the progress entry's "Known gaps" and (for locked decisions) in `DECISIONS.md`?
- User-facing language: matches the vocabulary the spec prescribes — no NAS terms, no internal jargon surfaced to users.

### 4. Go discipline

Hold every PR to `CLAUDE.md # Go code discipline`:

- **Layer boundaries** — `internal/store` imported only from `internal/lifecycle`, `internal/api`, `cmd/brain`. `internal/lifecycle` imported only from `internal/api` and `cmd/brain`. Nothing else reaches in.
- **Consumer-side interfaces** — interfaces declared in the package that uses them, not the one that implements them. No interface with fewer than two concrete consumers.
- **`log/slog` only** — no `"log"` imports, no `fmt.Println` for diagnostics. Structured fields, not interpolated strings.
- **Standard field names** — `instance_id`, `manifest_id`, `slug`, `service`, `image`, `host`, `upstream`, `step`, `err`, `output`, `user_id`, `username`, `role`, `action`, `actor_user_id`, `target_kind`, `target_id`.
- **Typed errors only at boundaries** — define a sentinel or typed error only when a consumer needs to discriminate (HTTP status, retry decision, UI text). No speculative error types.
- **No premature abstraction** — no new interface, factory, or helper until at least two concrete consumers exist.

### 5. Audit completeness

Any handler that creates, deletes, role-changes, or password-changes a principal, or installs, uninstalls, or changes permissions on an app, must emit `audit.Record(...)` on **every observable failure path** (host 502, store 500, conflict 409, guard rejection) in addition to success. Check each path individually — never assume symmetry between similar handlers.

Pure reads and 422 validation failures do not audit.

### 6. Test coverage

- Are the tests exercising what they claim? Read the test name, then read the assertion — do they match?
- Adversarial paths covered: wrong user, missing resource, permission denied, duplicate/conflict, concurrent mutation.
- Store tests: multi-user isolation — one user's data not visible to another.
- API tests: 401 fence on every route, audience scoping over the wire.
- No test that passes vacuously: assertion on a zero value that was never populated, or an error check that can never fail.
- **Did the PR delete or disable a test?** Read the deletion count on `*_test.go` in the diff, not the suite result. Deleted tests never fail, so removing coverage — by accident or otherwise — leaves the check green. Overwriting a test file wholesale is the common accident: a directory listing that filters out `_test.go` hides the file about to be replaced.
- **Does every new test actually execute in CI?** A test gated on a file, binary, or environment that CI lacks skips on every run and reads exactly like a passing one. Skips are legitimate on this repo (root, `avahi-resolve`, NetworkManager, `bash`) — what is not legitimate is a skip guarding something that always exists, such as a git-tracked file, because that is a no-op dressed as coverage. CI lists every skipped test in the job summary (`ci-go.yml` # List skipped tests); check the new test is not in it.

### 7. Documentation honesty

- Does the progress entry's "what was done" match what the diff actually shows? Every "handled" claim must be verifiable in the code.
- "Known gaps & deviations": is it honest? Any path the tests don't cover, spec item deferred without being named, or integration test skipped because the environment didn't support it must be named explicitly with the reason.
- Index entries updated: `docs/progress/README.md` and `docs/README.md` both reflect the new progress doc.

### 8. Scope

- Does the diff contain only what the issue requires? Any change not explainable by the issue title is suspect.
- Drive-by reformatting, renaming, or refactoring of code outside the issue scope is a Block — it conflates review signals and breaks blame/bisect.
- Out-of-scope findings: if the contributor fixed something unrelated, check whether a separate issue was filed for it. If not, the fix should be reverted and a new issue opened. Maintainers decide what gets built and when.

### 9. Migration safety

- Schema changes must be additive: new columns, new tables, new indexes only. No `DROP COLUMN`, `RENAME COLUMN`, or destructive `ALTER TABLE`.
- New columns on existing tables must have a `DEFAULT` or be nullable — the migration runs against a live database with existing rows.
- `ON DELETE CASCADE` foreign key constraints must be intentional and called out in the PR body.

### 10. Error message quality

- User-facing error messages (those that reach API responses or the dashboard) must be plain English. Raw Go error strings must not reach the client.
- Internal errors belong in logs; translated messages belong in responses. Verify the translation happens at the API boundary, not deeper in the stack.

### 11. New dependencies

- Any new entry in `go.mod`/`go.sum` or `package.json`/`package-lock.json` must be called out in the PR body with the reason.
- Flag if the dependency is large, unmaintained, or has a license incompatible with the project.

### 12. Commit hygiene

- Commits should be logical units, not a record of the agent's trial-and-error process (no "fix", "fix again", "oops", "wip" commits).
- Each commit message states the *why*, not the what — the diff already shows the what.
- No commits that only fix formatting or linting that `make fmt`/`make check` would have caught before the first substantive commit.

## Output format

One finding per item, structured as:

```
[Block|Note|Question] <lens name> — <one-line summary>
<file:line if applicable>
<explanation and suggested fix or path to one>
```

Group Blocks first, then Notes, then Questions. Omit empty groups.

End with a one-line verdict: **approved**, **approved with notes** (no Blocks, Notes only), or **changes required** (one or more Blocks).
