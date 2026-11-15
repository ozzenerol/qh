package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ff(rel, before, now string) fileFact {
	kind := "modified"
	if before == "" {
		kind = "created"
	}
	return fileFact{rel: rel, kind: kind, before: before, now: now, add: 1}
}

func TestNeverRules(t *testing.T) {
	notes := "## Imports\n- never: `org.testcontainers.containers.PostgreSQLContainer` (the 1.x class)\n- Never: WebSecurityConfigurerAdapter\nnot a never: rule here\n"
	got := neverRules(notes)
	if len(got) != 2 || got[0] != "org.testcontainers.containers.PostgreSQLContainer" || got[1] != "WebSecurityConfigurerAdapter" {
		t.Fatalf("rules %q", got)
	}
	added := addedLines([]fileFact{ff("src/T.java", "package a;\n", "package a;\nimport org.testcontainers.containers.PostgreSQLContainer;\n")})
	hits := neverHits(got, added)
	if len(hits) != 1 || !strings.Contains(hits[0], "src/T.java:2") {
		t.Fatalf("hits %q", hits)
	}
}

func TestAddedLinesIgnoresMovedLines(t *testing.T) {
	a := addedLines([]fileFact{ff("x.go", "a\nb\nc\n", "c\na\nb\nd\n")})
	if len(a["x.go"]) != 1 || strings.TrimSpace(a["x.go"][0].text) != "d" || a["x.go"][0].n != 4 {
		t.Fatalf("%+v", a)
	}
}

const brief08 = "API:\n```java\n// BorrowOutcome.java\npublic sealed interface BorrowOutcome {\n    record Lent(long loanId, LocalDate dueOn) implements BorrowOutcome {}\n    record Refused(BorrowDecision.Reason reason) implements BorrowOutcome {}\n}\n// BorrowService.java\npublic BorrowOutcome borrow(long memberId, long titleId, long branchId)\n```\n" +
	"```java\n@Query(\"\"\"\n        select c.currentBranch.id as branchId, count(c) as available\n        from Copy c\n        \"\"\")\nList<BranchAvailability> availabilityByBranch(@Param(\"titleId\") Long titleId);\n```\n" +
	"```java\n@Bean\n@ServiceConnection\nPostgreSQLContainer postgres() {\n    return new PostgreSQLContainer(\"postgres:17-alpine\");\n}\ncopyRepository.saveAndFlush(copy);\nClock c = Clock.systemUTC();\n```\n```sql\ncreate table x (id bigint);\n```\n"

func TestAPILines(t *testing.T) {
	got := apiLines(brief08)
	want := []string{
		"public sealed interface BorrowOutcome {",
		"record Lent(long loanId, LocalDate dueOn) implements BorrowOutcome {}",
		"record Refused(BorrowDecision.Reason reason) implements BorrowOutcome {}",
		"public BorrowOutcome borrow(long memberId, long titleId, long branchId)",
		`List<BranchAvailability> availabilityByBranch(@Param("titleId") Long titleId);`,
		"PostgreSQLContainer postgres() {",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMissingAPI(t *testing.T) {
	copied := map[string]string{"BorrowOutcome.java": "public sealed interface BorrowOutcome {\n  record Lent(long loanId,\n      LocalDate dueOn) implements BorrowOutcome {}\n  record Refused(Reason reason) implements BorrowOutcome {}\n}\n",
		"BorrowService.java": "public BorrowOutcome borrow(long memberId, long titleId, long branchId) {}\n" +
			"List<BranchAvailability> availabilityByBranch(@Param(\"titleId\") Long titleId);\nPostgreSQLContainer postgres() { }\n"}
	miss := missingAPI(brief08, copied)
	if len(miss) != 1 || !strings.Contains(miss[0], "Refused(BorrowDecision.Reason") {
		t.Fatalf("missing %q", miss)
	}
}

func TestCleanupFindings(t *testing.T) {
	src := `package a;

import org.springframework.transaction.PlatformTransactionManager;

class S {
    private final org.springframework.transaction.PlatformTransactionManager tm;

    @SuppressWarnings("unchecked")
    private Lent lend(Title title, Member member) {
        Copy unused = repo.find(member);
        Copy used = repo.find(member);
        Copy kept = tt.execute(status -> {
            return repo.find(member);
        });
        kept.lend();
        return new Lent(used.getId());
    }

    private <T> T commitSave(T entity, JpaRepository<T, ?> repo) {
        return repo.save(entity);
    }

    String q() {
        return """
            select c from Copy c where c.status = org.example.app.CopyStatus.AVAILABLE
            """;
    }
}
`
	fs := []fileFact{ff("src/S.java", "", src), ff("src/Reason.java", "", "package a;\nenum Reason { A, B,\n C }\n")}
	srcs := map[string]string{"src/S.java": src, "src/Reason.java": fs[1].now, "domain/Decision.java": "enum Reason { C, A, B }\n"}
	added := addedLines(fs)
	now := map[string]string{"src/S.java": src, "src/Reason.java": fs[1].now}

	fq := fqnInCode(added, now)
	if len(fq) != 1 || !strings.Contains(fq[0], "src/S.java:6") {
		t.Fatalf("fqn %q", fq)
	}
	un := unusedNames(added, now)
	if len(un) != 2 || !strings.Contains(un[0], "parameter title of lend") || !strings.Contains(un[1], "local unused") {
		t.Fatalf("unused %q", un)
	}
	ce := copiedEnums(added, srcs)
	if len(ce) != 1 || !strings.Contains(ce[0], "domain/Decision.java") {
		t.Fatalf("enums %q", ce)
	}
	if s := suppressions(added); len(s) != 1 {
		t.Fatalf("suppress %q", s)
	}
}

func TestGuardMessage(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "A.java"), []byte("x"), 0o644)
	src := "class A {\n    void f() {\n        // Load entities\n        a();\n        // Gather inputs\n        b();\n        // the cap keeps 91.25 at 20.00\n        c();\n    }\n}\n"
	prompt := "Change: A.java\nnever: `com.fasterxml.jackson.databind`\n"
	msg, found := guard(prompt, dir, nil, []fileFact{ff("A.java", "", src), ff("mvnw", "", "#!/bin/sh\n")})
	for _, w := range []string{"outside the task's Change list:\nmvnw", "You added these comments:", "A.java:3 // Load entities"} {
		if !strings.Contains(msg, w) {
			t.Fatalf("message lacks %q:\n%s", w, msg)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %q", found)
	}
	if msg, _ := guard(prompt, dir, nil, []fileFact{ff("A.java", "", "class A {}\n")}); msg != "" {
		t.Fatalf("clean change gave %q", msg)
	}
}

// QH_GUARD_REPO=dir QH_GUARD_BRIEF=file go test -run TestGuardOnRepo -v
// prints what the guard says about a whole repo as if every file were new.
func TestGuardOnRepo(t *testing.T) {
	repo, brief := os.Getenv("QH_GUARD_REPO"), os.Getenv("QH_GUARD_BRIEF")
	if repo == "" || brief == "" {
		t.Skip("set QH_GUARD_REPO and QH_GUARD_BRIEF")
	}
	b, _ := os.ReadFile(brief)
	var fs []fileFact
	for rel, src := range repoSources(repo, 3000) {
		if strings.Contains(string(b), filepath.Base(rel)) {
			fs = append(fs, ff(rel, "", src))
		}
	}
	msg, _ := guard(string(b), repo, nil, fs)
	t.Log("\n" + msg)
}

func TestBriefAllowsReflection(t *testing.T) {
	dir := t.TempDir()
	if briefAllowsReflection("Set ids in tests with plain constructors.", dir) {
		t.Fatal("no reflection named, but allowed")
	}
	if !briefAllowsReflection("Set the entity id with ReflectionTestUtils.setField(branch, \"id\", 1L).", dir) {
		t.Fatal("brief names ReflectionTestUtils, but not allowed")
	}
}
