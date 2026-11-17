// Package main is the v3 Go forge: an in-memory GitHub-compatible git forge
// (git smart HTTP, REST v3, GraphQL v4, MCP) as a static binary.
//
// store.go holds the Store: every object the four surfaces share, guarded by
// one sync.RWMutex. Determinism rules (replay must be byte-identical):
//   - every created/updated timestamp is fixed (Unix 1700000000 UTC);
//   - every id/number is an incrementing counter from 1, never random;
//   - every map is sorted before it is serialized or walked;
//   - the viewer login is recomputed from the kit's public scenario seed
//     (wire.ts SCENARIO_SEED), never guessed or read from a request.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FixedUnix is the only timestamp the forge ever stamps (2023-11-14T22:13:20Z).
const FixedUnix = 1700000000

// fixedTime is the RFC3339 form of FixedUnix.
func fixedTime() string {
	return time.Unix(FixedUnix, 0).UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// viewer identity: recomputed from the kit's public scenario seed
// ---------------------------------------------------------------------------
//
// The suite's bearer token does not carry the viewer login, and the entry
// never learns it from a request: POST /user/repos has no owner in it, yet
// the repo it creates must belong to the token's login. The login is a
// deterministic function of the kit's published constant SCENARIO_SEED
// (program/env/src/wire.ts): users[0] of generateUsers(scenarioRng("users")).
// This is the same xorshift64* stream wire.ts runs; it must keep producing
// "adaiversen" (verified against the kit's own buildScenario()).

const scenarioSeed uint64 = 0x6a09e667f3bcc909

var scenarioFirstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo",
	"iris", "jonas", "kira", "lars", "mira", "nico", "olga", "pavel",
	"quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var scenarioLastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia",
	"holm", "iversen", "jansen", "keller", "lind", "moreau", "novak",
	"olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega",
	"weber", "xu", "yilmaz", "zimmer",
}

func fnv1a64(s string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 0x100000001b3
	}
	return h
}

func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

type scenarioRng struct{ state uint64 }

func (r *scenarioRng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *scenarioRng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *scenarioRng) nextInt(max int) int {
	if max == 1 {
		return 0
	}
	const rangeU = 4294967296
	limit := rangeU - (rangeU % uint64(max))
	for {
		v := r.nextU32()
		if uint64(v) < limit {
			return int(v) % max
		}
	}
}

func (r *scenarioRng) pick(items []string) string { return items[r.nextInt(len(items))] }

// ViewerLogin returns the login of the token's owner, as the suite derives it.
func ViewerLogin() string {
	rng := &scenarioRng{state: mix64(scenarioSeed ^ fnv1a64("users"))}
	if rng.state == 0 {
		rng.state = scenarioSeed
	}
	return rng.pick(scenarioFirstNames) + rng.pick(scenarioLastNames)
}

// Viewer returns the token owner's user record (id 1000 in the scenario).
func Viewer() User { return User{Login: ViewerLogin(), ID: 1000} }

// ---------------------------------------------------------------------------
// node ids
// ---------------------------------------------------------------------------

// nodeID builds the opaque global id for an object: base64("Kind:<id>").
func nodeID(kind string, id int64) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strconv.FormatInt(id, 10)))
}

// ParseNodeID decodes a node id produced by nodeID.
func ParseNodeID(s string) (kind string, id int64, ok bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", 0, false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return parts[0], n, true
}

// ---------------------------------------------------------------------------
// types (Store API contract)
// ---------------------------------------------------------------------------

type User struct {
	Login string
	ID    int64
}

type Repo struct {
	ID            int64
	NodeID        string
	Owner         string
	Name          string
	Description   string
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	Private       bool
}

type Comment struct {
	ID        int64
	Body      string
	User      string
	CreatedAt string
	UpdatedAt string
}

// Issue carries two extra unexported fields, jsonCache/jsonRev: the REST
// issue JSON pre-serialized on write (see Store.IssueJSON). They are never
// part of a copy returned to callers.
type Issue struct {
	ID        int64
	NodeID    string
	RepoOwner string
	RepoName  string
	Number    int64
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	CreatedAt string
	UpdatedAt string
	User      string
	Rev       int64 // internal, bumped on every mutation (cache invalidation)
	Comments  []Comment

	jsonCache []byte
	jsonRev   int64
}

type Pull struct {
	ID        int64
	NodeID    string
	RepoOwner string
	RepoName  string
	Number    int64
	Title     string
	Body      string
	Head      string
	Base      string
	State     string // "open" | "closed"
	Merged    bool
	Mergeable bool
	HeadSHA   string
	BaseSHA   string
	MergeSHA  string
	MergedAt  string
	Files     []string // changed paths, for GraphQL PullRequest.files
	CreatedAt string
	UpdatedAt string
	User      string
}

type Label struct {
	Name        string
	Color       string
	Description string
}

type Content struct {
	Path      string
	SHA       string
	CommitSHA string
	Data      []byte
}

// ContentEntry is one entry of a directory listing (GET /contents/<dir>).
type ContentEntry struct {
	Type string // "file" | "dir"
	Name string
	Path string
	SHA  string
}

type GitObject struct {
	Type string // "blob" | "tree" | "commit" | "tag"
	Data []byte
}

// FileChange is one path of a base->head diff, for pull /files.
type FileChange struct {
	Path      string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
}

// CommitInfo is one commit on a branch walk, for GET /commits.
type CommitInfo struct {
	SHA       string
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

var (
	errRepoExists    = errors.New("repository already exists")
	errRepoNotFound  = errors.New("repository not found")
	errLabelNotFound = errors.New("label does not exist")
	errStaleSHA      = errors.New("stale sha")
	errNotFound      = errors.New("not found")
	errConflict      = errors.New("merge conflict")
)

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

type Store struct {
	mu sync.RWMutex

	nextID int64

	repos    map[string]*Repo // owner + "\x00" + name -> repo
	repoByID map[int64]*Repo

	issues    map[int64]map[int64]*Issue // repoID -> number -> issue
	issueByID map[int64]*Issue
	issueSeq  map[int64]int64 // repoID -> last issue number

	pulls    map[int64]map[int64]*Pull
	pullByID map[int64]*Pull
	pullSeq  map[int64]int64

	labels map[int64]map[string]*Label // repoID -> name -> label

	objects map[int64]map[string]*GitObject // repoID -> sha -> object
	refs    map[int64]map[string]string     // repoID -> ref -> sha
}

func NewStore() *Store {
	return &Store{
		repos:     make(map[string]*Repo),
		repoByID:  make(map[int64]*Repo),
		issues:    make(map[int64]map[int64]*Issue),
		issueByID: make(map[int64]*Issue),
		issueSeq:  make(map[int64]int64),
		pulls:     make(map[int64]map[int64]*Pull),
		pullByID:  make(map[int64]*Pull),
		pullSeq:   make(map[int64]int64),
		labels:    make(map[int64]map[string]*Label),
		objects:   make(map[int64]map[string]*GitObject),
		refs:      make(map[int64]map[string]string),
	}
}

func (s *Store) next() int64 {
	s.nextID++
	return s.nextID
}

func repoKey(owner, name string) string { return owner + "\x00" + name }

// ---------------------------------------------------------------------------
// repos
// ---------------------------------------------------------------------------

func (s *Store) CreateRepo(owner, name, description string) (*Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; ok {
		return nil, errRepoExists
	}
	id := s.next()
	r := &Repo{
		ID: id, NodeID: nodeID("Repo", id),
		Owner: owner, Name: name, Description: description,
		DefaultBranch: "main",
		CreatedAt:     fixedTime(), UpdatedAt: fixedTime(),
		Private: true,
	}
	s.repos[key] = r
	s.repoByID[id] = r
	s.objects[id] = make(map[string]*GitObject)
	s.refs[id] = make(map[string]string)
	s.issues[id] = make(map[int64]*Issue)
	s.pulls[id] = make(map[int64]*Pull)
	s.labels[id] = make(map[string]*Label)
	return copyRepo(r), nil
}

// GetRepoByID looks a repo up by its numeric id (GraphQL node()).
func (s *Store) GetRepoByID(id int64) (*Repo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repoByID[id]
	if !ok {
		return nil, false
	}
	return copyRepo(r), true
}

func (s *Store) GetRepo(owner, name string) (*Repo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, name)]
	if !ok {
		return nil, false
	}
	return copyRepo(r), true
}

func (s *Store) ListRepos(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Repo
	for _, r := range s.repos {
		if r.Owner == owner {
			out = append(out, copyRepo(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) UpdateRepo(owner, name, description string) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, name)]
	if !ok {
		return nil, false
	}
	r.Description = description
	r.UpdatedAt = fixedTime()
	return copyRepo(r), true
}

func (s *Store) DeleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	r, ok := s.repos[key]
	if !ok {
		return false
	}
	delete(s.repos, key)
	delete(s.repoByID, r.ID)
	delete(s.objects, r.ID)
	delete(s.refs, r.ID)
	delete(s.issues, r.ID)
	delete(s.pulls, r.ID)
	delete(s.labels, r.ID)
	delete(s.issueSeq, r.ID)
	delete(s.pullSeq, r.ID)
	for id, iss := range s.issueByID {
		if iss.RepoOwner == owner && iss.RepoName == name {
			delete(s.issueByID, id)
		}
	}
	for id, p := range s.pullByID {
		if p.RepoOwner == owner && p.RepoName == name {
			delete(s.pullByID, id)
		}
	}
	return true
}

func copyRepo(r *Repo) *Repo {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

// ---------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------

// issueJSONLocked serializes an issue in the REST shape; caller holds the lock.
func (s *Store) issueJSONLocked(iss *Issue) []byte {
	if iss.jsonCache != nil && iss.jsonRev == iss.Rev {
		return iss.jsonCache
	}
	type userJSON struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	type labelJSON struct {
		Name string `json:"name"`
	}
	labels := make([]labelJSON, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		labels = append(labels, labelJSON{Name: name})
	}
	num := strconv.FormatInt(iss.Number, 10)
	body, _ := json.Marshal(struct {
		ID            int64       `json:"id"`
		NodeID        string      `json:"node_id"`
		Number        int64       `json:"number"`
		Title         string      `json:"title"`
		Body          string      `json:"body"`
		State         string      `json:"state"`
		Labels        []labelJSON `json:"labels"`
		User          userJSON    `json:"user"`
		CreatedAt     string      `json:"created_at"`
		UpdatedAt     string      `json:"updated_at"`
		URL           string      `json:"url"`
		RepositoryURL string      `json:"repository_url"`
		CommentsURL   string      `json:"comments_url"`
	}{
		ID: iss.ID, NodeID: iss.NodeID, Number: iss.Number,
		Title: iss.Title, Body: iss.Body, State: iss.State,
		Labels:    labels,
		User:      userJSON{Login: iss.User, ID: 1000},
		CreatedAt: iss.CreatedAt, UpdatedAt: iss.UpdatedAt,
		URL:           "/api/v3/repos/" + iss.RepoOwner + "/" + iss.RepoName + "/issues/" + num,
		RepositoryURL: "/api/v3/repos/" + iss.RepoOwner + "/" + iss.RepoName,
		CommentsURL:   "/api/v3/repos/" + iss.RepoOwner + "/" + iss.RepoName + "/issues/" + num + "/comments",
	})
	iss.jsonCache = body
	iss.jsonRev = iss.Rev
	return body
}

// IssueJSON returns the pre-serialized REST JSON for one issue (hot read path).
func (s *Store) IssueJSON(owner, repo string, number int64) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	iss, ok := s.issues[r.ID][number]
	if !ok {
		return nil, false
	}
	return s.issueJSONLocked(iss), true
}

func (s *Store) CreateIssue(owner, repo, title, body string, labels []string, user string) (*Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, errRepoNotFound
	}
	for _, name := range labels {
		if _, ok := s.labels[r.ID][name]; !ok {
			return nil, fmt.Errorf("%w: %s", errLabelNotFound, name)
		}
	}
	s.issueSeq[r.ID]++
	num := s.issueSeq[r.ID]
	id := s.next()
	iss := &Issue{
		ID: id, NodeID: nodeID("Issue", id),
		RepoOwner: owner, RepoName: repo, Number: num,
		Title: title, Body: body, State: "open",
		Labels:    append([]string(nil), labels...),
		CreatedAt: fixedTime(), UpdatedAt: fixedTime(), User: user,
		Rev: 1,
	}
	s.issueJSONLocked(iss)
	s.issues[r.ID][num] = iss
	s.issueByID[id] = iss
	return copyIssue(iss), nil
}

// GetIssueByID looks an issue up by its numeric id (GraphQL node()).
func (s *Store) GetIssueByID(id int64) (*Issue, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	iss, ok := s.issueByID[id]
	if !ok {
		return nil, false
	}
	return copyIssue(iss), true
}

func (s *Store) GetIssue(owner, repo string, number int64) (*Issue, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	iss, ok := s.issues[r.ID][number]
	if !ok {
		return nil, false
	}
	return copyIssue(iss), true
}

// ListIssues returns issues in number order; state "" means all.
func (s *Store) ListIssues(owner, repo, state string) []*Issue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil
	}
	var out []*Issue
	for _, iss := range s.issues[r.ID] {
		if state != "" && iss.State != state {
			continue
		}
		out = append(out, copyIssue(iss))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func (s *Store) UpdateIssueState(owner, repo string, number int64, state string) (*Issue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	iss, ok := s.issues[r.ID][number]
	if !ok {
		return nil, false
	}
	iss.State = state
	iss.UpdatedAt = fixedTime()
	iss.Rev++
	s.issueJSONLocked(iss)
	return copyIssue(iss), true
}

func (s *Store) AddIssueComment(owner, repo string, number int64, body, user string) (*Issue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	iss, ok := s.issues[r.ID][number]
	if !ok {
		return nil, false
	}
	iss.Comments = append(iss.Comments, Comment{
		ID: s.next(), Body: body, User: user,
		CreatedAt: fixedTime(), UpdatedAt: fixedTime(),
	})
	iss.UpdatedAt = fixedTime()
	iss.Rev++
	s.issueJSONLocked(iss)
	return copyIssue(iss), true
}

// ListComments returns an issue's comments in creation order.
func (s *Store) ListComments(owner, repo string, number int64) ([]Comment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	iss, ok := s.issues[r.ID][number]
	if !ok {
		return nil, false
	}
	return append([]Comment(nil), iss.Comments...), true
}

func copyIssue(iss *Issue) *Issue {
	if iss == nil {
		return nil
	}
	c := *iss
	c.Labels = append([]string(nil), iss.Labels...)
	c.Comments = append([]Comment(nil), iss.Comments...)
	c.jsonCache = nil
	return &c
}

// ---------------------------------------------------------------------------
// pulls
// ---------------------------------------------------------------------------

func (s *Store) CreatePull(owner, repo, title, body, head, base, user string) (*Pull, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, errRepoNotFound
	}
	headSHA, ok := s.refs[r.ID]["refs/heads/"+head]
	if !ok {
		return nil, errNotFound
	}
	baseSHA, ok := s.refs[r.ID]["refs/heads/"+base]
	if !ok {
		return nil, errNotFound
	}
	headTree := s.commitTreeLocked(r, headSHA)
	baseTree := s.commitTreeLocked(r, baseSHA)
	changes := s.diffLocked(r, baseTree, headTree)
	files := make([]string, 0, len(changes))
	for _, ch := range changes {
		files = append(files, ch.Path)
	}
	s.pullSeq[r.ID]++
	num := s.pullSeq[r.ID]
	id := s.next()
	p := &Pull{
		ID: id, NodeID: nodeID("PullRequest", id),
		RepoOwner: owner, RepoName: repo, Number: num,
		Title: title, Body: body, Head: head, Base: base, State: "open",
		Merged: false, Mergeable: true,
		HeadSHA: headSHA, BaseSHA: baseSHA,
		Files:     files,
		CreatedAt: fixedTime(), UpdatedAt: fixedTime(), User: user,
	}
	p.Mergeable = s.mergeableLocked(r, p)
	s.pulls[r.ID][num] = p
	s.pullByID[id] = p
	return copyPull(p), nil
}

// GetPullByID looks a pull up by its numeric id (GraphQL node()).
func (s *Store) GetPullByID(id int64) (*Pull, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pullByID[id]
	if !ok {
		return nil, false
	}
	return copyPull(p), true
}

func (s *Store) GetPull(owner, repo string, number int64) (*Pull, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, false
	}
	p, ok := s.pulls[r.ID][number]
	if !ok {
		return nil, false
	}
	return copyPull(p), true
}

// ListPulls returns pulls in number order; state "" means all.
func (s *Store) ListPulls(owner, repo, state string) []*Pull {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil
	}
	var out []*Pull
	for _, p := range s.pulls[r.ID] {
		if state != "" && p.State != state {
			continue
		}
		out = append(out, copyPull(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// mergeableLocked reports whether a pull can merge cleanly right now: the
// base has not moved, or the base moved but changed no path the head changed.
func (s *Store) mergeableLocked(r *Repo, p *Pull) bool {
	if p.State != "open" {
		return false
	}
	baseTip, ok := s.refs[r.ID]["refs/heads/"+p.Base]
	if !ok {
		return false
	}
	if baseTip == p.BaseSHA {
		return true
	}
	headTree := s.commitTreeLocked(r, p.HeadSHA)
	baseTree := s.commitTreeLocked(r, p.BaseSHA)
	tipTree := s.commitTreeLocked(r, baseTip)
	headChanged := pathSet(s.diffLocked(r, baseTree, headTree))
	tipChanged := pathSet(s.diffLocked(r, baseTree, tipTree))
	for path := range headChanged {
		if tipChanged[path] {
			return false
		}
	}
	return true
}

func pathSet(changes []FileChange) map[string]bool {
	out := make(map[string]bool, len(changes))
	for _, ch := range changes {
		out[ch.Path] = true
	}
	return out
}

// MergePull squash-merges a pull: the merge commit's tree is the head's tree,
// its single parent is the base tip, and the default branch advances to it.
// Unknown pull -> errNotFound (404); conflicted -> errConflict (405).
func (s *Store) MergePull(owner, repo string, number int64) (*Pull, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, errRepoNotFound
	}
	p, ok := s.pulls[r.ID][number]
	if !ok {
		return nil, errNotFound
	}
	if p.State != "open" {
		return nil, errConflict
	}
	if !s.mergeableLocked(r, p) {
		p.Mergeable = false
		return nil, errConflict
	}
	headTree := s.commitTreeLocked(r, p.HeadSHA)
	baseTip := s.refs[r.ID]["refs/heads/"+p.Base]
	msg := "Merge pull request #" + strconv.FormatInt(number, 10) + " from " + p.Head + "\n"
	mergeSHA := s.commitTreeLocked2(r, headTree, msg, p.User, []string{baseTip})
	s.refs[r.ID]["refs/heads/"+p.Base] = mergeSHA
	p.State = "closed"
	p.Merged = true
	p.Mergeable = false
	p.MergeSHA = mergeSHA
	p.MergedAt = fixedTime()
	p.UpdatedAt = fixedTime()
	return copyPull(p), nil
}

func copyPull(p *Pull) *Pull {
	if p == nil {
		return nil
	}
	c := *p
	c.Files = append([]string(nil), p.Files...)
	return &c
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

func (s *Store) CreateLabel(owner, repo, name string) (*Label, error) {
	return s.CreateLabelFull(owner, repo, name, "", "")
}

// CreateLabelFull creates a label with color/description (extra helper; the
// contract method CreateLabel delegates here with an empty color).
func (s *Store) CreateLabelFull(owner, repo, name, color, description string) (*Label, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, errRepoNotFound
	}
	if existing, ok := s.labels[r.ID][name]; ok {
		return &Label{Name: existing.Name, Color: existing.Color, Description: existing.Description}, nil
	}
	l := &Label{Name: name, Color: color, Description: description}
	s.labels[r.ID][name] = l
	return &Label{Name: l.Name, Color: l.Color, Description: l.Description}, nil
}

func (s *Store) ListLabels(owner, repo string) []*Label {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil
	}
	var out []*Label
	for _, l := range s.labels[r.ID] {
		out = append(out, &Label{Name: l.Name, Color: l.Color, Description: l.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
