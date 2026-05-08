package enjugit

import (
	"errors"
	"os"
	"sort"
	"sync"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// fakeOps is a recording mock implementing git.Ops. Workflow
// tests use it to assert which git ops were invoked, in what
// order, with what arguments — without spinning up a real repo.
type fakeOps struct {
	mu sync.Mutex

	// Recording
	calls []fakeCall

	// Canned responses (set by test before invocation).
	headSHA     string
	headBranch  string
	resolveMap  map[string]string // refName → SHA
	state       git.WorktreeState
	branches    []string
	readContent map[string][]byte // sha+path → content

	// Tree-read fakes for Workflow methods that read templates,
	// bundles, or arbitrary subtrees from a commit's tree.
	treeEntries map[string][]git.TreeEntry                                            // sha+":"+dirPath → entries (nil = ok=false)
	walkBlobs   map[string]map[string]struct{ Mode os.FileMode; Content []byte }     // sha+":"+dirPath → relPath → blob

	// Commit-walk fake for WalkRecentCommits. Visited newest-first;
	// the slice is treated as "most recent at index 0".
	recentCommits []struct{ SHA, Message string }

	// Errors to inject.
	errOnCall map[string]error // method name → error to return

	// commitFailAfter, when > 0, makes the (commitFailAfter+1)th
	// CommitFiles call (1-indexed) and every subsequent call
	// return commitFailErr. Used by batch tests to drive a mid-
	// loop commit failure after some entries have already
	// committed (so rollback has work to do).
	commitFailAfter int
	commitFailErr   error
	commitCallCount int

	// Tracking for WithLock reentrancy assertion.
	insideWithLock bool

	// workDir lets tests exercise the Workflow.WorkDir() type-
	// assertion path. Empty (the default) means the fake doesn't
	// expose a WorkDir method, which is the production-test
	// behavior; setting it makes the fake satisfy the WorkDir()
	// interface so workflow can read it.
	workDir string

	// checkoutMissingUntilCreated mimics "branch doesn't exist
	// yet, so Checkout fails with ErrRefNotFound, until a
	// CreateBranchAt for the same name happens — after which
	// Checkout succeeds." Lets prepareBranchForCommit tests
	// drive the full multi-step chain (skipped local → tracked
	// origin → forked from default) without standing up a real
	// repo.
	checkoutMissingUntilCreated string
	checkoutCreatedBranches     map[string]bool

	// IsAncestor canned response. ancestorReturnSet=false (zero
	// value) means "default to true" — the happy path for
	// prepareBranchForCommit's validate-stale-ref step. Tests that
	// want to drive the reseat branch flip ancestorReturnSet=true
	// + ancestorReturn=false.
	ancestorReturnSet bool
	ancestorReturn    bool
}

// WorkDir returns the configured test workdir or "". Workflow
// reaches it via type assertion on git.Ops, so tests that need
// the worktree-fallback path set fake.workDir before calling
// the verb under test.
func (f *fakeOps) WorkDir() string { return f.workDir }

type fakeCall struct {
	Method string
	Args   []interface{}
}

func newFakeOps() *fakeOps {
	return &fakeOps{
		resolveMap:  make(map[string]string),
		readContent: make(map[string][]byte),
		errOnCall:   make(map[string]error),
		treeEntries: make(map[string][]git.TreeEntry),
		walkBlobs:   make(map[string]map[string]struct{ Mode os.FileMode; Content []byte }),
		state:       git.StateClean,
		headSHA:     "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		headBranch:  "main",
	}
}

func (f *fakeOps) record(method string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{Method: method, Args: args})
}

func (f *fakeOps) inject(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errOnCall[method] = err
}

func (f *fakeOps) checkErr(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.errOnCall[method]
}

func (f *fakeOps) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func (f *fakeOps) lastCall(method string) *fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Method == method {
			c := f.calls[i]
			return &c
		}
	}
	return nil
}

// --- Ops interface impl ---

func (f *fakeOps) ReadFileAtCommit(sha, path string) ([]byte, bool, error) {
	f.record("ReadFileAtCommit", sha, path)
	if err := f.checkErr("ReadFileAtCommit"); err != nil {
		return nil, false, err
	}
	c, ok := f.readContent[sha+":"+path]
	return c, ok, nil
}

func (f *fakeOps) ResolveRef(name string) (string, error) {
	f.record("ResolveRef", name)
	if err := f.checkErr("ResolveRef"); err != nil {
		return "", err
	}
	if sha, ok := f.resolveMap[name]; ok {
		return sha, nil
	}
	return "", git.ErrRefNotFound
}

func (f *fakeOps) Head() (string, string, error) {
	f.record("Head")
	return f.headSHA, f.headBranch, f.checkErr("Head")
}

func (f *fakeOps) LocalBranches() ([]string, error) {
	f.record("LocalBranches")
	return f.branches, f.checkErr("LocalBranches")
}

func (f *fakeOps) State() git.WorktreeState {
	f.record("State")
	return f.state
}

func (f *fakeOps) ReadTreeEntriesAtCommit(sha, dirPath string) ([]git.TreeEntry, bool, error) {
	f.record("ReadTreeEntriesAtCommit", sha, dirPath)
	if err := f.checkErr("ReadTreeEntriesAtCommit"); err != nil {
		return nil, false, err
	}
	entries, ok := f.treeEntries[sha+":"+dirPath]
	return entries, ok, nil
}

func (f *fakeOps) WalkSubtreeBlobsAtCommit(sha, dirPath string, visit git.BlobVisitor) error {
	f.record("WalkSubtreeBlobsAtCommit", sha, dirPath)
	if err := f.checkErr("WalkSubtreeBlobsAtCommit"); err != nil {
		return err
	}
	blobs, ok := f.walkBlobs[sha+":"+dirPath]
	if !ok {
		return nil
	}
	// Walk in deterministic order so test assertions are stable.
	keys := make([]string, 0, len(blobs))
	for k := range blobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, rel := range keys {
		b := blobs[rel]
		if err := visit(rel, b.Mode, b.Content); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeOps) LogFile(relPath string) ([]git.CommitInfo, error) {
	f.record("LogFile", relPath)
	return nil, f.checkErr("LogFile")
}

func (f *fakeOps) WalkRecentCommits(maxWalk int, visit func(sha, message string) bool) error {
	f.record("WalkRecentCommits", maxWalk)
	if err := f.checkErr("WalkRecentCommits"); err != nil {
		return err
	}
	for i, c := range f.recentCommits {
		if maxWalk > 0 && i >= maxWalk {
			break
		}
		if !visit(c.SHA, c.Message) {
			return nil
		}
	}
	return nil
}

func (f *fakeOps) CreateBranchAt(name, baseSHA string) error {
	f.record("CreateBranchAt", name, baseSHA)
	if err := f.checkErr("CreateBranchAt"); err != nil {
		return err
	}
	// Mark this branch as "now exists" so subsequent Checkout
	// hits the success path under checkoutMissingUntilCreated.
	if f.checkoutCreatedBranches == nil {
		f.checkoutCreatedBranches = map[string]bool{}
	}
	f.checkoutCreatedBranches[name] = true
	if _, exists := f.resolveMap["refs/heads/"+name]; exists {
		return git.ErrBranchExists
	}
	f.resolveMap["refs/heads/"+name] = baseSHA
	return nil
}

func (f *fakeOps) DeleteBranch(name string) error {
	f.record("DeleteBranch", name)
	delete(f.resolveMap, "refs/heads/"+name)
	return f.checkErr("DeleteBranch")
}

func (f *fakeOps) SetBranchTo(name, sha string) error {
	f.record("SetBranchTo", name, sha)
	if err := f.checkErr("SetBranchTo"); err != nil {
		return err
	}
	f.resolveMap["refs/heads/"+name] = sha
	return nil
}

// IsAncestor stub: defaults to true (the common case — a stale-
// ref test that wants to assert the reseat path runs explicitly
// flips ancestorReturn to false). Tests that don't care about
// ancestry get the no-op-reseat behavior the production happy
// path takes.
func (f *fakeOps) IsAncestor(ancestor, descendant string) (bool, error) {
	f.record("IsAncestor", ancestor, descendant)
	if err := f.checkErr("IsAncestor"); err != nil {
		return false, err
	}
	if f.ancestorReturnSet {
		return f.ancestorReturn, nil
	}
	return true, nil
}

func (f *fakeOps) Checkout(branch string) error {
	f.record("Checkout", branch)
	if err := f.checkErr("Checkout"); err != nil {
		return err
	}
	// Branch-missing-until-created hook for prepareBranchForCommit
	// tests. Returns ErrRefNotFound on Checkout if the branch
	// matches checkoutMissingUntilCreated AND CreateBranchAt
	// hasn't recorded it yet.
	if f.checkoutMissingUntilCreated != "" && branch == f.checkoutMissingUntilCreated {
		f.mu.Lock()
		created := f.checkoutCreatedBranches[branch]
		f.mu.Unlock()
		if !created {
			return git.ErrRefNotFound
		}
	}
	return nil
}

func (f *fakeOps) CheckoutCommit(sha string) error {
	f.record("CheckoutCommit", sha)
	if err := f.checkErr("CheckoutCommit"); err != nil {
		return err
	}
	f.state = git.StateDetached
	return nil
}

func (f *fakeOps) ResetClean() error {
	f.record("ResetClean")
	if err := f.checkErr("ResetClean"); err != nil {
		return err
	}
	f.state = git.StateClean
	return nil
}

func (f *fakeOps) RemoveFiles(paths []string) error {
	f.record("RemoveFiles", paths)
	return f.checkErr("RemoveFiles")
}

func (f *fakeOps) CommitFiles(req git.CommitRequest) (git.CommitResult, error) {
	f.record("CommitFiles", req)
	if err := f.checkErr("CommitFiles"); err != nil {
		return git.CommitResult{}, err
	}
	f.mu.Lock()
	f.commitCallCount++
	count := f.commitCallCount
	failAfter := f.commitFailAfter
	failErr := f.commitFailErr
	f.mu.Unlock()
	if failAfter > 0 && count > failAfter {
		return git.CommitResult{}, failErr
	}
	return git.CommitResult{SHA: "newsha000000000000000000000000000000000000"}, nil
}

func (f *fakeOps) Push(branch string) error {
	f.record("Push", branch)
	return f.checkErr("Push")
}

func (f *fakeOps) CheckoutBranchFrom(branch, baseBranch, defaultBranch string) error {
	f.record("CheckoutBranchFrom")
	return f.checkErr("CheckoutBranchFrom")
}

func (f *fakeOps) RebaseOnRemote(branch string) error {
	f.record("RebaseOnRemote")
	return f.checkErr("RebaseOnRemote")
}

func (f *fakeOps) PushAllRefs(force bool) error {
	f.record("PushAllRefs")
	return f.checkErr("PushAllRefs")
}

func (f *fakeOps) PushWithVerify(branch, expected string) error {
	f.record("PushWithVerify", branch, expected)
	return f.checkErr("PushWithVerify")
}

func (f *fakeOps) Fetch() error {
	f.record("Fetch")
	return f.checkErr("Fetch")
}

func (f *fakeOps) EnsureOrigin(url string) error {
	f.record("EnsureOrigin", url)
	return f.checkErr("EnsureOrigin")
}

func (f *fakeOps) RemoveOrigin() error {
	f.record("RemoveOrigin")
	return f.checkErr("RemoveOrigin")
}

func (f *fakeOps) FetchBranch(branch string) error {
	f.record("FetchBranch", branch)
	return f.checkErr("FetchBranch")
}

func (f *fakeOps) PullBranch(branch string) error {
	f.record("PullBranch", branch)
	return f.checkErr("PullBranch")
}

func (f *fakeOps) LocalBranchHash(branch string) (string, error) {
	f.record("LocalBranchHash", branch)
	if err := f.checkErr("LocalBranchHash"); err != nil {
		return "", err
	}
	if sha, ok := f.resolveMap["refs/heads/"+branch]; ok {
		return sha, nil
	}
	if sha, ok := f.resolveMap["refs/remotes/origin/"+branch]; ok {
		return sha, nil
	}
	return "", nil
}

func (f *fakeOps) ScanBranchSince(branch, since string, visit func(sha, message string)) (string, error) {
	f.record("ScanBranchSince", branch, since)
	if err := f.checkErr("ScanBranchSince"); err != nil {
		return since, err
	}
	for _, c := range f.recentCommits {
		visit(c.SHA, c.Message)
	}
	if len(f.recentCommits) > 0 {
		return f.recentCommits[0].SHA, nil
	}
	return since, nil
}

func (f *fakeOps) ReadFile(path string) ([]byte, error) {
	f.record("ReadFile", path)
	return nil, f.checkErr("ReadFile")
}

func (f *fakeOps) CheckoutBranch(branch string) error {
	f.record("CheckoutBranch", branch)
	if branch == "" {
		return nil
	}
	return f.Checkout(branch)
}

func (f *fakeOps) MergeFFOrFail(target, source string) (string, error) {
	f.record("MergeFFOrFail", target, source)
	if err := f.checkErr("MergeFFOrFail"); err != nil {
		return "", err
	}
	return "ffsha", nil
}

func (f *fakeOps) MergeWithCommit(target, source, msg, name, email string) (string, error) {
	f.record("MergeWithCommit", target, source, msg, name, email)
	if err := f.checkErr("MergeWithCommit"); err != nil {
		return "", err
	}
	return "mergesha", nil
}

func (f *fakeOps) CompareToRemote(branches []string) (*git.RemoteComparison, error) {
	f.record("CompareToRemote", branches)
	return &git.RemoteComparison{}, f.checkErr("CompareToRemote")
}

func (f *fakeOps) WithLock(fn func(git.Ops) error) error {
	f.record("WithLock")
	if f.insideWithLock {
		return errors.New("nested WithLock without reentrancy support in fake")
	}
	f.insideWithLock = true
	defer func() { f.insideWithLock = false }()
	return fn(f)
}
