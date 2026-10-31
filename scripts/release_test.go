// Package scripts tests the helper scripts used to build and release abp.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a throwaway working clone with a bare "origin" beside it, so the
// release script can fetch, compare and push without touching anything real.
// Origin starts with one commit on main, tagged v0.1.0 and v0.1.1.
type repo struct {
	t      *testing.T
	work   string // the clone the script runs in
	remote string // the bare origin
	seed   string // a second clone, used to put new commits on origin
}

// run executes a command in dir and fails the test if it does not succeed. Git
// is isolated from the developer's own configuration.
func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	out, err := command(dir, env, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// command builds an exec.Cmd with a clean, git-friendly environment plus extra.
func command(dir string, extra []string, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}, extra...)
	return cmd
}

// newRepo builds the origin, the seed clone and the working clone.
func newRepo(t *testing.T) *repo {
	t.Helper()
	for _, bin := range []string{"git", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	root := t.TempDir()
	r := &repo{
		t:      t,
		remote: filepath.Join(root, "origin.git"),
		seed:   filepath.Join(root, "seed"),
		work:   filepath.Join(root, "work"),
	}
	run(t, root, nil, "git", "init", "--quiet", "--bare", "-b", "main", r.remote)
	run(t, root, nil, "git", "clone", "--quiet", r.remote, r.seed)
	run(t, r.seed, nil, "git", "checkout", "--quiet", "-b", "main")
	r.commit(r.seed, "one")
	run(t, r.seed, nil, "git", "tag", "-a", "v0.1.0", "-m", "v0.1.0")
	r.commit(r.seed, "two")
	run(t, r.seed, nil, "git", "tag", "-a", "v0.1.1", "-m", "v0.1.1")
	run(t, r.seed, nil, "git", "push", "--quiet", "origin", "main", "--tags")
	run(t, root, nil, "git", "clone", "--quiet", r.remote, r.work)
	return r
}

// commit adds a new commit to the clone at dir.
func (r *repo) commit(dir, msg string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, msg+".txt"), []byte(msg), 0o644); err != nil {
		r.t.Fatal(err)
	}
	run(r.t, dir, nil, "git", "add", ".")
	run(r.t, dir, nil, "git", "commit", "--quiet", "-m", msg)
}

// release runs the script in the working clone. SKIP_CHECKS and YES are on by
// default so tests need not have a Makefile or a terminal; extra overrides them.
func (r *repo) release(extra []string, arg string) (string, error) {
	r.t.Helper()
	script, err := filepath.Abs("release.sh")
	if err != nil {
		r.t.Fatal(err)
	}
	env := append([]string{"SKIP_CHECKS=1", "YES=1"}, extra...)
	cmd := command(r.work, env, "sh", script, arg)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// tags lists the tags known to dir's repository.
func (r *repo) tags(dir string) string {
	return run(r.t, dir, nil, "git", "tag", "--list")
}

// TestReleaseCreatesAndPushesAnnotatedTag is the happy path: the tag appears
// locally and on origin, is annotated, and points at the current commit.
func TestReleaseCreatesAndPushesAnnotatedTag(t *testing.T) {
	r := newRepo(t)
	r.commit(r.seed, "three")
	run(t, r.seed, nil, "git", "push", "--quiet", "origin", "main")
	run(t, r.work, nil, "git", "pull", "--quiet")

	out, err := r.release(nil, "v0.2.0")
	if err != nil {
		t.Fatalf("release failed: %v\n%s", err, out)
	}
	if got := run(t, r.work, nil, "git", "cat-file", "-t", "v0.2.0"); got != "tag" {
		t.Errorf("tag type = %q, want an annotated tag", got)
	}
	head := run(t, r.work, nil, "git", "rev-parse", "HEAD")
	if got := run(t, r.work, nil, "git", "rev-parse", "v0.2.0^{commit}"); got != head {
		t.Errorf("tag points at %s, want HEAD %s", got, head)
	}
	if !strings.Contains(run(t, r.remote, nil, "git", "tag", "--list"), "v0.2.0") {
		t.Error("tag was not pushed to origin")
	}
	if !strings.Contains(out, "1 commit(s) since v0.1.1") {
		t.Errorf("summary should count commits since the last release:\n%s", out)
	}
}

// TestReleaseBumps checks patch/minor/major compute from the latest stable tag,
// ignoring pre-releases, and start from v0.0.0 when there are no tags.
func TestReleaseBumps(t *testing.T) {
	for arg, want := range map[string]string{"patch": "v0.1.2", "minor": "v0.2.0", "major": "v1.0.0"} {
		t.Run(arg, func(t *testing.T) {
			r := newRepo(t)
			// A pre-release newer than the stable tag must not be the base.
			run(t, r.seed, nil, "git", "tag", "v0.5.0-rc1")
			run(t, r.seed, nil, "git", "push", "--quiet", "origin", "--tags")
			if out, err := r.release(nil, arg); err != nil {
				t.Fatalf("release %s failed: %v\n%s", arg, err, out)
			}
			if !strings.Contains(r.tags(r.work), want) {
				t.Errorf("after %s, tags = %q, want %s", arg, r.tags(r.work), want)
			}
		})
	}

	t.Run("no tags yet", func(t *testing.T) {
		r := newRepo(t)
		run(t, r.seed, nil, "git", "push", "--quiet", "origin", ":refs/tags/v0.1.0", ":refs/tags/v0.1.1")
		run(t, r.work, nil, "git", "tag", "-d", "v0.1.0", "v0.1.1")
		if out, err := r.release(nil, "patch"); err != nil {
			t.Fatalf("release failed: %v\n%s", err, out)
		}
		if !strings.Contains(r.tags(r.work), "v0.0.1") {
			t.Errorf("tags = %q, want v0.0.1", r.tags(r.work))
		}
	})
}

// TestReleasePrerelease checks a -rc tag is accepted for a new version.
func TestReleasePrerelease(t *testing.T) {
	r := newRepo(t)
	if out, err := r.release(nil, "v0.2.0-rc1"); err != nil {
		t.Fatalf("release failed: %v\n%s", err, out)
	}
	if !strings.Contains(run(t, r.remote, nil, "git", "tag", "--list"), "v0.2.0-rc1") {
		t.Error("pre-release tag was not pushed")
	}
}

// TestReleaseDryRun checks a rehearsal runs the checks but changes nothing.
func TestReleaseDryRun(t *testing.T) {
	r := newRepo(t)
	out, err := r.release([]string{"DRY_RUN=1"}, "v0.2.0")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Nothing was changed") {
		t.Errorf("dry run should say it changed nothing:\n%s", out)
	}
	if strings.Contains(r.tags(r.work), "v0.2.0") || strings.Contains(run(t, r.remote, nil, "git", "tag", "--list"), "v0.2.0") {
		t.Error("dry run must not create a tag")
	}
}

// TestReleaseRefuses covers every guard: each case must fail with a message
// naming the problem and leave no new tag locally or on origin.
func TestReleaseRefuses(t *testing.T) {
	cases := []struct {
		name  string
		arg   string
		env   []string
		setup func(r *repo)
		want  string // text expected in the error output
	}{
		{name: "no leading v", arg: "1.2.3", want: "not a valid version"},
		{name: "missing patch", arg: "v1.2", want: "not a valid version"},
		{name: "junk suffix", arg: "v1.2.3+build", want: "not a valid version"},
		{name: "empty suffix", arg: "v1.2.3-", want: "not a valid version"},
		{name: "already exists", arg: "v0.1.1", want: "already exists"},
		{name: "older than latest", arg: "v0.0.9", want: "older than the latest release"},
		{name: "pre-release of a shipped version", arg: "v0.1.1-rc1", want: "already released"},
		{
			name: "exists only on origin", arg: "v0.3.0", want: "already exists",
			setup: func(r *repo) {
				// Pushed from elsewhere after the clone was made: the script's
				// fetch must notice it.
				run(r.t, r.seed, nil, "git", "tag", "v0.3.0")
				run(r.t, r.seed, nil, "git", "push", "--quiet", "origin", "v0.3.0")
			},
		},
		{
			name: "wrong branch", arg: "v0.2.0", want: "releases are made from 'main'",
			setup: func(r *repo) { run(r.t, r.work, nil, "git", "checkout", "--quiet", "-b", "feature") },
		},
		{
			name: "detached head", arg: "v0.2.0", want: "detached HEAD",
			setup: func(r *repo) { run(r.t, r.work, nil, "git", "checkout", "--quiet", "--detach") },
		},
		{
			name: "untracked file", arg: "v0.2.0", want: "uncommitted or untracked",
			setup: func(r *repo) {
				if err := os.WriteFile(filepath.Join(r.work, "new.txt"), nil, 0o644); err != nil {
					r.t.Fatal(err)
				}
			},
		},
		{
			name: "modified file", arg: "v0.2.0", want: "uncommitted or untracked",
			setup: func(r *repo) {
				if err := os.WriteFile(filepath.Join(r.work, "two.txt"), []byte("changed"), 0o644); err != nil {
					r.t.Fatal(err)
				}
			},
		},
		{
			name: "behind origin", arg: "v0.2.0", want: "is behind origin/main",
			setup: func(r *repo) {
				r.commit(r.seed, "three")
				run(r.t, r.seed, nil, "git", "push", "--quiet", "origin", "main")
			},
		},
		{
			name: "unpushed commits", arg: "v0.2.0", want: "differs from origin/main",
			setup: func(r *repo) { r.commit(r.work, "local-only") },
		},
		{name: "no confirmation available", arg: "v0.2.0", env: []string{"YES="}, want: "set YES=1"},
		{name: "unknown remote", arg: "v0.2.0", env: []string{"REMOTE=nope"}, want: "no git remote named 'nope'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRepo(t)
			if c.setup != nil {
				c.setup(r)
			}
			before := run(t, r.remote, nil, "git", "tag", "--list")
			out, err := r.release(c.env, c.arg)
			if err == nil {
				t.Fatalf("expected a refusal, got success:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("error should mention %q:\n%s", c.want, out)
			}
			if after := run(t, r.remote, nil, "git", "tag", "--list"); after != before {
				t.Errorf("origin tags changed from %q to %q", before, after)
			}
			if strings.Contains(r.tags(r.work), "v0.2.0") {
				t.Error("a local v0.2.0 tag was left behind")
			}
		})
	}
}

// TestReleaseUsage checks the argument count is enforced.
func TestReleaseUsage(t *testing.T) {
	r := newRepo(t)
	script, _ := filepath.Abs("release.sh")
	out, err := command(r.work, nil, "sh", script).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "usage:") {
		t.Errorf("no argument should print usage and fail: %v\n%s", err, out)
	}
}

// TestReleaseOutsideGitRepo checks a clear error when not in a repository.
func TestReleaseOutsideGitRepo(t *testing.T) {
	script, _ := filepath.Abs("release.sh")
	out, err := command(t.TempDir(), []string{"GIT_CEILING_DIRECTORIES=/"}, "sh", script, "patch").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "not inside a git repository") {
		t.Errorf("expected a not-a-repo error: %v\n%s", err, out)
	}
}

// TestReleaseBranchDetection checks the release branch is found without origin's
// HEAD (falling back to main/master), and that BRANCH overrides detection.
func TestReleaseBranchDetection(t *testing.T) {
	t.Run("fallback to main", func(t *testing.T) {
		r := newRepo(t)
		run(t, r.work, nil, "git", "remote", "set-head", "origin", "--delete")
		if out, err := r.release(nil, "v0.2.0"); err != nil {
			t.Fatalf("release failed: %v\n%s", err, out)
		}
	})
	t.Run("explicit BRANCH", func(t *testing.T) {
		r := newRepo(t)
		run(t, r.work, nil, "git", "checkout", "--quiet", "-b", "other")
		run(t, r.work, nil, "git", "push", "--quiet", "origin", "other")
		if out, err := r.release([]string{"BRANCH=other"}, "v0.2.0"); err != nil {
			t.Fatalf("release failed: %v\n%s", err, out)
		}
	})
	t.Run("undetectable", func(t *testing.T) {
		r := newRepo(t)
		run(t, r.work, nil, "git", "remote", "set-head", "origin", "--delete")
		run(t, r.remote, nil, "git", "branch", "-m", "main", "trunk")
		run(t, r.work, nil, "git", "fetch", "--prune", "--quiet")
		out, err := r.release(nil, "v0.2.0")
		if err == nil || !strings.Contains(out, "set BRANCH=<name>") {
			t.Errorf("expected a request to set BRANCH: %v\n%s", err, out)
		}
	})
}

// TestReleasePushFailureRemovesLocalTag checks that when origin rejects the tag
// the local one is removed, so the release can simply be retried.
func TestReleasePushFailureRemovesLocalTag(t *testing.T) {
	r := newRepo(t)
	hook := filepath.Join(r.remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := r.release(nil, "v0.2.0")
	if err == nil || !strings.Contains(out, "the local tag was removed") {
		t.Fatalf("expected a push failure message: %v\n%s", err, out)
	}
	if strings.Contains(r.tags(r.work), "v0.2.0") {
		t.Error("the local tag should have been removed after the failed push")
	}
}

// TestReleaseRunsChecks checks the CI checks gate the release: they are called
// with the expected targets, and a failure stops everything before tagging.
func TestReleaseRunsChecks(t *testing.T) {
	fake := func(t *testing.T, exit int) (script, log string) {
		dir := t.TempDir()
		log = filepath.Join(dir, "args")
		script = filepath.Join(dir, "fakemake")
		body := "#!/bin/sh\necho \"$@\" > " + log + "\nexit " + string(rune('0'+exit)) + "\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return script, log
	}

	t.Run("passing", func(t *testing.T) {
		r := newRepo(t)
		script, log := fake(t, 0)
		if out, err := r.release([]string{"SKIP_CHECKS=", "MAKE_CMD=" + script}, "v0.2.0"); err != nil {
			t.Fatalf("release failed: %v\n%s", err, out)
		}
		args, _ := os.ReadFile(log)
		if strings.TrimSpace(string(args)) != "fmt-check vet test-race" {
			t.Errorf("checks ran %q, want the CI targets", args)
		}
	})
	t.Run("failing", func(t *testing.T) {
		r := newRepo(t)
		script, _ := fake(t, 1)
		out, err := r.release([]string{"SKIP_CHECKS=", "MAKE_CMD=" + script}, "v0.2.0")
		if err == nil || !strings.Contains(out, "checks failed; nothing was tagged") {
			t.Fatalf("expected the failing checks to stop the release: %v\n%s", err, out)
		}
		if strings.Contains(r.tags(r.work), "v0.2.0") {
			t.Error("no tag may be created when the checks fail")
		}
	})
}
