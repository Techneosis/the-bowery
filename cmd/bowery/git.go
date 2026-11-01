package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitOut runs git in repo on the current server. ok is false when git exited
// non-zero; err is only for git (or ssh) not running at all.
func gitOut(repo string, args ...string) (out string, ok bool, err error) {
	if machine != "" {
		// On the box, as the agent's identity when there is one (fetch and push
		// authenticate as them).
		rc, o, err := sshSh(identityPrefix() + shJoin(append([]string{"git", "-C", repo}, args...)...))
		if err != nil {
			return "", false, err
		}
		return strings.TrimSpace(o), rc == 0, nil
	}
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	e := cmd.Run()
	if e != nil {
		if _, isExit := e.(*exec.ExitError); !isExit {
			return "", false, fail("git: %v", e)
		}
		msg := stderr.String()
		if msg == "" {
			msg = stdout.String()
		}
		return strings.TrimSpace(msg), false, nil
	}
	return strings.TrimSpace(stdout.String()), true, nil
}

// git runs a git command that must succeed.
func git(repo string, args ...string) (string, error) {
	out, ok, err := gitOut(repo, args...)
	if err != nil {
		return "", err
	}
	if !ok {
		where := ""
		if machine != "" {
			where = " on " + machine
		}
		return "", fail("git %s%s: %s", strings.Join(args, " "), where, out)
	}
	return out, nil
}

// gitMaybe is git whose failure is an answer: the output, or "" and false.
func gitMaybe(repo string, args ...string) (string, bool, error) {
	out, ok, err := gitOut(repo, args...)
	if !ok {
		out = ""
	}
	return out, ok, err
}

type wtInfo struct {
	repo, top, branch string
}

// worktreeInfo describes a linked worktree (main checkout, checkout top,
// branch or ""), or nil for anything else.
func worktreeInfo(path string) (*wtInfo, error) {
	d, err := isDir(path)
	if err != nil || !d {
		return nil, err
	}
	top, _, err := gitMaybe(path, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	gd, _, err := gitMaybe(path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	common, _, err := gitMaybe(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if top == "" || gd == "" || common == "" || filepath.Clean(gd) == filepath.Clean(common) {
		return nil, nil
	}
	branch, _, err := gitMaybe(path, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		return nil, err
	}
	return &wtInfo{repo: filepath.Dir(filepath.Clean(common)), top: filepath.Clean(top), branch: branch}, nil
}

func mainCheckout(path string) (string, error) {
	common, _, err := gitMaybe(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return "", err
	}
	return filepath.Dir(filepath.Clean(common)), nil
}

func hasOrigin(repo string) (bool, error) {
	out, _, err := gitMaybe(repo, "remote")
	for _, r := range strings.Fields(out) {
		if r == "origin" {
			return true, err
		}
	}
	return false, err
}

// defaultBranch is the ref work is merged into: origin's HEAD, else
// origin/main|master, else main|master; "" for none.
func defaultBranch(repo string) (string, error) {
	o, err := hasOrigin(repo)
	if err != nil {
		return "", err
	}
	if o {
		if head, _, err := gitMaybe(repo, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err != nil || head != "" {
			return head, err
		}
		for _, b := range []string{"origin/main", "origin/master"} {
			if _, ok, err := gitMaybe(repo, "rev-parse", "-q", "--verify", b); err != nil || ok {
				return b, err
			}
		}
	}
	for _, b := range []string{"main", "master"} {
		if _, ok, err := gitMaybe(repo, "rev-parse", "-q", "--verify", "refs/heads/"+b); err != nil || ok {
			return b, err
		}
	}
	return "", nil
}

func fetch(repo string) (bool, error) {
	o, err := hasOrigin(repo)
	if err != nil || !o {
		return err == nil, err
	}
	_, ok, err := gitOut(repo, "fetch", "-q", "--prune", "origin")
	return ok, err
}

// mergedInto: "ancestor", "content" (a squash or rebase merge: merging the
// branch changes nothing), or "" for not merged.
func mergedInto(repo, rev, target string) (string, error) {
	if _, ok, err := gitMaybe(repo, "merge-base", "--is-ancestor", rev, target); err != nil || ok {
		if ok {
			return "ancestor", err
		}
		return "", err
	}
	out, _, err := gitMaybe(repo, "merge-tree", "--write-tree", target, rev)
	if err != nil {
		return "", err
	}
	if f := strings.Fields(out); len(f) > 0 {
		tree, err := git(repo, "rev-parse", target+"^{tree}")
		if err != nil {
			return "", err
		}
		if f[0] == tree {
			return "content", nil
		}
	}
	return "", nil
}

func dirty(path string, untracked bool) (bool, error) {
	args := []string{"status", "--porcelain"}
	if !untracked {
		args = append(args, "--untracked-files=no")
	}
	out, err := git(path, args...)
	return out != "", err
}

func localWorktreeRoot() string {
	if r := os.Getenv("BOWERY_WORKTREE_ROOT"); r != "" {
		return r
	}
	text, _ := os.ReadFile(filepath.Join(home(), ".config", "herdr", "config.toml"))
	return herdrWorktreeDir(string(text), home())
}

func worktreeRoot() (string, error) {
	if machine != "" {
		r, err := remote()
		if err != nil {
			return "", err
		}
		return r.wtroot, nil
	}
	return localWorktreeRoot(), nil
}

func trustedRoots() ([]string, error) {
	wt, err := worktreeRoot()
	if err != nil {
		return nil, err
	}
	if machine != "" {
		r, _ := remote()
		var out []string
		for _, t := range machineConfig(machine).TrustRoots {
			if t == "~" || strings.HasPrefix(t, "~/") {
				t = r.home + t[1:]
			}
			out = append(out, t)
		}
		return append(out, wt), nil
	}
	var out []string
	for _, t := range cfg.TrustRoots {
		out = append(out, expandUser(t))
	}
	out = append(out, wt)
	for _, p := range strings.Split(os.Getenv("BOWERY_TRUST_ROOTS"), ":") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// linkedWorktree is an existing linked worktree the user named, as a record
// entry; refused for anything else.
func linkedWorktree(path string) (RecWorktree, error) {
	p, err := hostPath(path)
	if err != nil {
		return RecWorktree{}, err
	}
	info, err := worktreeInfo(p)
	if err != nil {
		return RecWorktree{}, err
	}
	if info == nil {
		return RecWorktree{}, refuse("%s is not a linked git worktree", p)
	}
	return RecWorktree{Path: info.top, Repo: info.repo, Branch: info.branch}, nil
}
