// rest.go -- REST v3 handlers (Worker 2).
//
// INTEGRATION CONTRACT (for the coordinator / Worker 1's main.go):
//   - Transport types come from http.go exactly as the design doc states:
//     Request{Method, Path, RawQuery string; Headers map[string]string (lower-cased keys); Body []byte}
//     ResponseWriter with methods Status(code int), Header(k, v string), Write([]byte) (int, error)
//     Handler func(w *ResponseWriter, r *Request)
//   - main.go must build one API with NewAPI(store) and route every request
//     whose path is /api/v3, /api/v3/*, /api/graphql or /mcp through
//     api.Handle(w, r). Handle returns false for any other path (git surface,
//     unknown paths) so the main router can answer those itself.
//   - Auth is enforced inside these handlers (Bearer MAKEFUN_SHOP_TOKEN).
//     The viewer login defaults to "adaiversen": the kit scenario's viewer,
//     deterministic from wire.ts's constant SCENARIO_SEED, identical in every
//     trial. Override with env MAKEFUN_VIEWER_LOGIN if the kit ever changes.
//   - Node IDs are computed here (base64 "Repo:<id>" etc.) and never taken
//     from Store's NodeID field, so GraphQL round-trips work regardless of
//     how Worker 1 formats NodeID.
//   - Issue comments live in this file's registry (Store has AddIssueComment
//     but no list accessor); labels' colors and repos' private flags live in
//     overlays here because Store.CreateRepo/CreateLabel take no such params.
//   - Contents PUT/DELETE are implemented here on the git tree
//     (PutObject/BuildTree/CommitTree/SetRef) so branch-scoped writes work;
//     Store.PutContent/DeleteContent take no branch and are not used.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// fixedTimeStr is the deterministic timestamp every object carries:
// time.Unix(1700000000, 0).UTC(), RFC3339. The Store uses the same instant.
const fixedTimeStr = "2023-11-14T22:13:20Z"

const viewerID = 1000 // scenario viewer id, same deterministic source
const emptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

type commentRec struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	User      string `json:"-"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type labelMeta struct {
	Color       string
	Description string
}

// API is the shared state for the REST, GraphQL and MCP surfaces.
type API struct {
	store  *Store
	token  string
	viewer string

	reqSeq int64 // X-GitHub-Request-Id counter

	mu         sync.Mutex
	comments   map[string][]*commentRec // owner\x00repo\x00number -> comments
	commentSeq int64
	priv       map[string]bool // owner\x00repo -> private flag from POST body
	labelMeta  map[string]labelMeta
}

// NewAPI builds the API surfaces over one Store. The viewer login comes from
// the scenario derivation (ViewerLogin), the same value the router uses, so
// REST, GraphQL and MCP always agree on who the token owner is.
func NewAPI(s *Store) *API {
	return &API{
		store:     s,
		token:     os.Getenv("MAKEFUN_SHOP_TOKEN"),
		viewer:    ViewerLogin(),
		comments:  make(map[string][]*commentRec),
		priv:      make(map[string]bool),
		labelMeta: make(map[string]labelMeta),
	}
}

// sharedAPI is the single API instance shared by the REST, GraphQL and MCP
// registrations. Comment registries and label/private overlays live on the
// API, so all three surfaces must share one instance or a comment created
// over GraphQL would be invisible over REST.
var sharedAPI *API

func shared(store *Store) *API {
	if sharedAPI == nil {
		sharedAPI = NewAPI(store)
	}
	return sharedAPI
}

// registerREST wires the REST v3 surface into the router. Auth and the
// X-GitHub-Request-Id stamp are the router's job (ServeRequest); the API
// dispatcher below only routes and answers. One catch-all per method is
// enough: the dispatcher re-parses the path itself.
func registerREST(rt *Router, store *Store) {
	api := shared(store)
	for _, method := range []string{"GET", "POST", "PATCH", "PUT", "DELETE"} {
		rt.Handle(method, "/api/v3/{rest...}", func(w *ResponseWriter, r *Request) {
			api.rest(w, r)
		})
	}
}

// ViewerLogin returns the login the bearer token owns.
func (a *API) ViewerLogin() string { return a.viewer }

// Handle dispatches one request to the REST, GraphQL or MCP surface.
// It returns false when the path belongs to no API surface.
func (a *API) Handle(w *ResponseWriter, r *Request) bool {
	p := r.Path
	if p == "/api/graphql" {
		a.graphql(w, r)
		return true
	}
	if p == "/mcp" {
		a.mcp(w, r)
		return true
	}
	if p == "/api/v3" || strings.HasPrefix(p, "/api/v3/") {
		a.rest(w, r)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Small helpers

func (a *API) nextReqID() string {
	n := atomic.AddInt64(&a.reqSeq, 1)
	return "req-" + strconv.FormatInt(n, 10)
}

// stampReqID stamps X-GitHub-Request-Id when the transport has not already
// done so (the router stamps it before routing; the stub transport does not).
func (a *API) stampReqID(w *ResponseWriter) {
	if _, ok := w.headers["x-github-request-id"]; !ok {
		w.Header("X-GitHub-Request-Id", a.nextReqID())
	}
}

// writeJSON sends a JSON reply with the required headers.
func (a *API) writeJSON(w *ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"message":"Internal Server Error"}`)
		status = 500
	}
	w.Header("Content-Type", "application/json")
	a.stampReqID(w)
	w.Status(status)
	_, _ = w.Write(b)
}

// writeErr sends {"message": ...} with the given status.
func (a *API) writeErr(w *ResponseWriter, status int, msg string) {
	if msg == "" {
		msg = "Not Found"
	}
	a.writeJSON(w, status, map[string]string{"message": msg})
}

// authed reports whether the request carries the right bearer token.
// An empty configured token disables auth, matching the router.
func (a *API) authed(r *Request) bool {
	if a.token == "" {
		return true
	}
	return r.Headers["authorization"] == "Bearer "+a.token
}

// requireAuth answers 401 when the token is missing or wrong.
func (a *API) requireAuth(w *ResponseWriter, r *Request) bool {
	if a.authed(r) {
		return true
	}
	a.writeErr(w, 401, "Requires authentication")
	return false
}

// parseBody decodes a JSON object body. An empty body yields an empty object.
// A malformed body yields ok=false (the caller answers 400).
func parseBody(r *Request) (map[string]any, bool) {
	body := bytes.TrimSpace(r.Body)
	if len(body) == 0 {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// requireBody parses the body or answers 400 JSON and reports false.
func (a *API) requireBody(w *ResponseWriter, r *Request) (map[string]any, bool) {
	m, ok := parseBody(r)
	if !ok {
		a.writeErr(w, 400, "Problems parsing JSON")
		return nil, false
	}
	return m, true
}

func bodyString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func bodyBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// bodyStringList reads a field that is either a list of strings or a list of
// objects with a "name" field (issue labels arrive as plain strings).
func bodyStringList(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if s, ok := t["name"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func queryOf(r *Request) url.Values {
	q, _ := url.ParseQuery(r.RawQuery)
	if q == nil {
		q = url.Values{}
	}
	return q
}

func apiSplitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	for i := range parts {
		if u, err := url.PathUnescape(parts[i]); err == nil {
			parts[i] = u
		}
	}
	return parts
}

func parseIntParam(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// pageParams reads ?page=&per_page= (GitHub defaults: page 1, per_page 30).
func pageParams(q url.Values) (page, perPage int) {
	page = int(parseIntParam(q.Get("page"), 1))
	if page < 1 {
		page = 1
	}
	perPage = int(parseIntParam(q.Get("per_page"), 30))
	if perPage < 1 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

// pageSlice returns the page's items for a list already in final order.
func pageSlice[T any](items []T, page, perPage int) []T {
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// linkNext builds a GitHub-style Link header for the next page, or "".
func (a *API) linkNext(r *Request, q url.Values, page, perPage, total int) string {
	if page*perPage >= total {
		return ""
	}
	host := r.Headers["host"]
	if host == "" {
		host = "127.0.0.1"
	}
	next := url.Values{}
	next.Set("per_page", strconv.Itoa(perPage))
	next.Set("page", strconv.Itoa(page+1))
	// Keep any other query params the caller sent (except page/per_page).
	for k, vs := range q {
		if k == "page" || k == "per_page" {
			continue
		}
		for _, v := range vs {
			next.Add(k, v)
		}
	}
	return "<http://" + host + r.Path + "?" + next.Encode() + `>; rel="next"`
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func b64enc(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

func b64dec(s string) ([]byte, error) {
	clean := strings.Map(func(c rune) rune {
		if c == '\r' || c == '\n' || c == '\t' || c == ' ' {
			return -1
		}
		return c
	}, s)
	return base64.StdEncoding.DecodeString(clean)
}

// ---------------------------------------------------------------------------
// Node IDs (opaque, deterministic, decodable by graphql.go)

func nodeIDRepo(id int64) string {
	return b64enc([]byte("Repo:" + strconv.FormatInt(id, 10)))
}

func nodeIDIssue(owner, repo string, number int64) string {
	return b64enc([]byte("Issue:" + owner + "/" + repo + ":" + strconv.FormatInt(number, 10)))
}

func nodeIDPull(owner, repo string, number int64) string {
	return b64enc([]byte("PullRequest:" + owner + "/" + repo + ":" + strconv.FormatInt(number, 10)))
}

func nodeIDComment(owner, repo string, issueNumber, commentID int64) string {
	return b64enc([]byte("IssueComment:" + owner + "/" + repo + ":" + strconv.FormatInt(issueNumber, 10) + ":" + strconv.FormatInt(commentID, 10)))
}

// decodeNodeID splits a node id this package minted into kind + payload.
func decodeNodeID(id string) (kind, payload string, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	s := string(raw)
	i := strings.Index(s, ":")
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// ---------------------------------------------------------------------------
// JSON shapes

type userJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type labelJSON struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type repoJSON struct {
	ID            int64    `json:"id"`
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	HTMLURL       string   `json:"html_url"`
	URL           string   `json:"url"`
}

type issueJSON struct {
	ID        int64       `json:"id"`
	NodeID    string      `json:"node_id"`
	Number    int64       `json:"number"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	State     string      `json:"state"`
	Labels    []labelJSON `json:"labels"`
	User      userJSON    `json:"user"`
	Comments  int         `json:"comments"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	URL       string      `json:"url"`
	HTMLURL   string      `json:"html_url"`
}

type refJSON struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label,omitempty"`
}

type pullJSON struct {
	ID             int64    `json:"id"`
	NodeID         string   `json:"node_id"`
	Number         int64    `json:"number"`
	State          string   `json:"state"`
	Title          string   `json:"title"`
	Body           string   `json:"body"`
	Head           refJSON  `json:"head"`
	Base           refJSON  `json:"base"`
	Merged         bool     `json:"merged"`
	Mergeable      bool     `json:"mergeable"`
	MergedAt       *string  `json:"merged_at"`
	MergeCommitSHA *string  `json:"merge_commit_sha"`
	User           userJSON `json:"user"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	URL            string   `json:"url"`
	HTMLURL        string   `json:"html_url"`
}

type commentJSON struct {
	ID        int64    `json:"id"`
	NodeID    string   `json:"node_id"`
	Body      string   `json:"body"`
	User      userJSON `json:"user"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func (a *API) repoOwner() userJSON { return userJSON{Login: a.viewer, ID: viewerID, Type: "User"} }

func (a *API) repoToJSON(repo *Repo) repoJSON {
	priv := false
	a.mu.Lock()
	if v, ok := a.priv[repo.Owner+"\x00"+repo.Name]; ok {
		priv = v
	}
	a.mu.Unlock()
	return repoJSON{
		ID: repo.ID, NodeID: nodeIDRepo(repo.ID), Name: repo.Name,
		FullName: repo.Owner + "/" + repo.Name, Owner: a.repoOwner(),
		Private: priv, Description: repo.Description,
		DefaultBranch: repo.DefaultBranch,
		CreatedAt:     repo.CreatedAt, UpdatedAt: repo.UpdatedAt,
		HTMLURL: "https://github.com/" + repo.Owner + "/" + repo.Name,
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
	}
}

func (a *API) issueToJSON(iss *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		labels = append(labels, a.labelForRepo(iss.RepoOwner, iss.RepoName, name))
	}
	nComments := len(a.listComments(iss.RepoOwner, iss.RepoName, iss.Number))
	return issueJSON{
		ID: iss.ID, NodeID: nodeIDIssue(iss.RepoOwner, iss.RepoName, iss.Number),
		Number: iss.Number, Title: iss.Title, Body: iss.Body, State: iss.State,
		Labels: labels, User: userJSON{Login: iss.User, ID: viewerID, Type: "User"},
		Comments: nComments, CreatedAt: iss.CreatedAt, UpdatedAt: iss.UpdatedAt,
		URL:     "/api/v3/repos/" + iss.RepoOwner + "/" + iss.RepoName + "/issues/" + strconv.FormatInt(iss.Number, 10),
		HTMLURL: "https://github.com/" + iss.RepoOwner + "/" + iss.RepoName + "/issues/" + strconv.FormatInt(iss.Number, 10),
	}
}

func (a *API) pullToJSON(p *Pull) pullJSON {
	var mergedAt *string
	var mergeSHA *string
	if p.Merged {
		s := fixedTimeStr
		mergedAt = &s
		if p.MergeSHA != "" {
			m := p.MergeSHA
			mergeSHA = &m
		}
	}
	return pullJSON{
		ID: p.ID, NodeID: nodeIDPull(p.RepoOwner, p.RepoName, p.Number),
		Number: p.Number, State: p.State, Title: p.Title, Body: p.Body,
		Head:   refJSON{Ref: p.Head, SHA: p.HeadSHA, Label: p.RepoOwner + ":" + p.Head},
		Base:   refJSON{Ref: p.Base, SHA: p.BaseSHA, Label: p.RepoOwner + ":" + p.Base},
		Merged: p.Merged, Mergeable: p.Mergeable,
		MergedAt: mergedAt, MergeCommitSHA: mergeSHA,
		User:      userJSON{Login: p.User, ID: viewerID, Type: "User"},
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		URL:     "/api/v3/repos/" + p.RepoOwner + "/" + p.RepoName + "/pulls/" + strconv.FormatInt(p.Number, 10),
		HTMLURL: "https://github.com/" + p.RepoOwner + "/" + p.RepoName + "/pull/" + strconv.FormatInt(p.Number, 10),
	}
}

// labelForRepo returns the label JSON for a label name in a repo, using the
// color/description recorded when the label was created via POST /labels.
func (a *API) labelForRepo(owner, repo, name string) labelJSON {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := owner + "\x00" + repo + "\x00" + name
	if meta, ok := a.labelMeta[key]; ok {
		return labelJSON{Name: name, Color: meta.Color, Description: meta.Description}
	}
	return labelJSON{Name: name, Color: "ededed"}
}

// ---------------------------------------------------------------------------
// Comment registry (shared by REST and GraphQL)

func commentKey(owner, repo string, number int64) string {
	return owner + "\x00" + repo + "\x00" + strconv.FormatInt(number, 10)
}

// addComment records a comment and bumps the issue in the Store. It returns
// the new comment id.
func (a *API) addComment(owner, repo string, number int64, body, user string) (int64, bool) {
	if _, ok := a.store.GetIssue(owner, repo, number); !ok {
		return 0, false
	}
	id := atomic.AddInt64(&a.commentSeq, 1)
	rec := &commentRec{ID: id, Body: body, User: user, CreatedAt: fixedTimeStr, UpdatedAt: fixedTimeStr}
	a.mu.Lock()
	key := commentKey(owner, repo, number)
	a.comments[key] = append(a.comments[key], rec)
	a.mu.Unlock()
	// Keep the Store's issue revision moving (cache invalidation there).
	a.store.AddIssueComment(owner, repo, number, body, user)
	return id, true
}

func (a *API) listComments(owner, repo string, number int64) []*commentRec {
	a.mu.Lock()
	defer a.mu.Unlock()
	src := a.comments[commentKey(owner, repo, number)]
	out := make([]*commentRec, len(src))
	copy(out, src)
	return out
}

func (a *API) commentToJSON(owner, repo string, number int64, c *commentRec) commentJSON {
	return commentJSON{
		ID: c.ID, NodeID: nodeIDComment(owner, repo, number, c.ID),
		Body: c.Body, User: userJSON{Login: c.User, ID: viewerID, Type: "User"},
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------
// REST router

func (a *API) rest(w *ResponseWriter, r *Request) {
	if !a.requireAuth(w, r) {
		return
	}
	// Strip the /api/v3 prefix; "" means the API root.
	rest := strings.TrimPrefix(r.Path, "/api/v3")
	rest = strings.TrimPrefix(rest, "/")
	segs := apiSplitPath(rest)
	method := r.Method

	// GET /api/v3/  (root) and GET /api/v3/meta
	if len(segs) == 0 {
		if method == "GET" {
			w.Header("Content-Type", "application/json")
			a.stampReqID(w)
			w.Header("X-OAuth-Scopes", "repo, user")
			w.Status(200)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		a.writeErr(w, 404, "Not Found")
		return
	}
	if len(segs) == 1 && segs[0] == "meta" {
		if method == "GET" {
			a.writeJSON(w, 200, map[string]string{"installed_version": "1.0.0"})
			return
		}
		a.writeErr(w, 404, "Not Found")
		return
	}
	if len(segs) == 1 && segs[0] == "user" {
		if method == "GET" {
			a.writeJSON(w, 200, a.repoOwner())
			return
		}
		a.writeErr(w, 404, "Not Found")
		return
	}
	if len(segs) == 2 && segs[0] == "user" && segs[1] == "repos" {
		switch method {
		case "GET":
			a.restListUserRepos(w, r)
		case "POST":
			a.restCreateRepo(w, r)
		default:
			a.writeErr(w, 404, "Not Found")
		}
		return
	}
	if len(segs) >= 3 && segs[0] == "repos" {
		a.restRepos(w, r, segs)
		return
	}
	a.writeErr(w, 404, "Not Found")
}

// restRepos handles /repos/{owner}/{repo}[/*].
func (a *API) restRepos(w *ResponseWriter, r *Request, segs []string) {
	owner, repoName := segs[1], segs[2]
	rest := segs[3:]
	method := r.Method

	// /repos/{owner}/{repo} itself: GET / PATCH / DELETE
	if len(rest) == 0 {
		switch method {
		case "GET":
			repo, ok := a.loadRepo(w, owner, repoName)
			if !ok {
				return
			}
			a.writeJSON(w, 200, a.repoToJSON(repo))
		case "PATCH":
			a.restPatchRepo(w, r, owner, repoName)
		case "DELETE":
			a.restDeleteRepo(w, owner, repoName)
		default:
			a.writeErr(w, 404, "Not Found")
		}
		return
	}

	switch rest[0] {
	case "branches":
		if method != "GET" {
			a.writeErr(w, 404, "Not Found")
			return
		}
		if len(rest) == 1 {
			a.restListBranches(w, owner, repoName)
			return
		}
		a.restGetBranch(w, owner, repoName, strings.Join(rest[1:], "/"))
	case "commits":
		if method == "GET" && len(rest) == 1 {
			a.restListCommits(w, r, owner, repoName)
			return
		}
		a.writeErr(w, 404, "Not Found")
	case "git":
		a.restGitRefs(w, r, owner, repoName, rest[2:])
	case "contents":
		a.restContents(w, r, owner, repoName, rest[1:])
	case "labels":
		if method == "POST" && len(rest) == 1 {
			a.restCreateLabel(w, r, owner, repoName)
			return
		}
		a.writeErr(w, 404, "Not Found")
	case "issues":
		a.restIssues(w, r, owner, repoName, rest[1:])
	case "pulls":
		a.restPulls(w, r, owner, repoName, rest[1:])
	default:
		a.writeErr(w, 404, "Not Found")
	}
}

// loadRepo enforces ownership (any other owner is 404, never 403) and
// existence. It answers the 404 itself and reports false when it does.
func (a *API) loadRepo(w *ResponseWriter, owner, name string) (*Repo, bool) {
	if owner != a.viewer {
		a.writeErr(w, 404, "Not Found")
		return nil, false
	}
	repo, ok := a.store.GetRepo(owner, name)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return nil, false
	}
	return repo, true
}

func (a *API) restListUserRepos(w *ResponseWriter, r *Request) {
	q := queryOf(r)
	page, perPage := pageParams(q)
	repos := a.store.ListRepos(a.viewer)
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	total := len(repos)
	items := pageSlice(repos, page, perPage)
	out := make([]repoJSON, 0, len(items))
	for _, repo := range items {
		out = append(out, a.repoToJSON(repo))
	}
	if next := a.linkNext(r, q, page, perPage, total); next != "" {
		w.Header("Link", next)
	}
	a.writeJSON(w, 200, out)
}

func (a *API) restCreateRepo(w *ResponseWriter, r *Request) {
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	name := bodyString(m, "name")
	if name == "" {
		a.writeErr(w, 422, "Repository name is required")
		return
	}
	if _, exists := a.store.GetRepo(a.viewer, name); exists {
		a.writeErr(w, 422, "Repository already exists")
		return
	}
	repo, err := a.store.CreateRepo(a.viewer, name, bodyString(m, "description"))
	if err != nil {
		a.writeErr(w, 422, "Repository could not be created")
		return
	}
	a.mu.Lock()
	a.priv[repo.Owner+"\x00"+repo.Name] = bodyBool(m, "private", false)
	a.mu.Unlock()
	a.writeJSON(w, 201, a.repoToJSON(repo))
}

func (a *API) restPatchRepo(w *ResponseWriter, r *Request, owner, name string) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	desc := bodyString(m, "description")
	// Only a description update is supported by the Store; other fields are
	// accepted and ignored, matching the checks (description only).
	repo, ok := a.store.UpdateRepo(owner, name, desc)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeJSON(w, 200, a.repoToJSON(repo))
}

func (a *API) restDeleteRepo(w *ResponseWriter, owner, name string) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	if !a.store.DeleteRepo(owner, name) {
		a.writeErr(w, 404, "Not Found")
		return
	}
	// Drop this repo's overlays (comments, labels, private flag).
	prefix := owner + "\x00" + name + "\x00"
	a.mu.Lock()
	for k := range a.comments {
		if strings.HasPrefix(k, prefix) {
			delete(a.comments, k)
		}
	}
	for k := range a.labelMeta {
		if strings.HasPrefix(k, prefix) {
			delete(a.labelMeta, k)
		}
	}
	delete(a.priv, owner+"\x00"+name)
	a.mu.Unlock()
	w.Header("Content-Type", "application/json")
	w.Header("X-GitHub-Request-Id", a.nextReqID())
	w.Status(204)
	_, _ = w.Write([]byte{})
}

// ---------------------------------------------------------------------------
// Branches, commits, git refs

type branchJSON struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

func (a *API) restListBranches(w *ResponseWriter, owner, name string) {
	repo, ok := a.loadRepo(w, owner, name)
	if !ok {
		return
	}
	refs := a.store.ListRefs(repo)
	names := make([]string, 0, len(refs))
	for ref := range refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	out := make([]branchJSON, 0, len(names))
	for _, n := range names {
		var b branchJSON
		b.Name = n
		b.Commit.SHA = refs["refs/heads/"+n]
		out = append(out, b)
	}
	a.writeJSON(w, 200, out)
}

func (a *API) restGetBranch(w *ResponseWriter, owner, name, branch string) {
	repo, ok := a.loadRepo(w, owner, name)
	if !ok {
		return
	}
	sha, ok := a.store.GetRef(repo, "refs/heads/"+branch)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	var b branchJSON
	b.Name = branch
	b.Commit.SHA = sha
	a.writeJSON(w, 200, b)
}

type commitJSON struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string  `json:"message"`
		Author    sigJSON `json:"author"`
		Committer sigJSON `json:"committer"`
	} `json:"commit"`
}

type sigJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func (a *API) restListCommits(w *ResponseWriter, r *Request, owner, name string) {
	repo, ok := a.loadRepo(w, owner, name)
	if !ok {
		return
	}
	q := queryOf(r)
	tip, ok := a.resolveRef(repo, q.Get("sha"))
	if !ok {
		a.writeJSON(w, 200, []commitJSON{})
		return
	}
	// Depth-first walk from the tip, parents in listed order, de-duplicated.
	seen := map[string]bool{}
	order := []string{}
	stack := []string{tip}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		if _, ok := a.store.GetObject(repo, sha); !ok {
			continue
		}
		order = append(order, sha)
		parents := a.commitParents(repo, sha)
		for i := len(parents) - 1; i >= 0; i-- {
			stack = append(stack, parents[i])
		}
	}
	out := make([]commitJSON, 0, len(order))
	for _, sha := range order {
		var c commitJSON
		c.SHA = sha
		c.Commit.Message = a.commitMessage(repo, sha)
		sig := sigJSON{Name: a.viewer, Email: a.viewer + "@example.invalid", Date: fixedTimeStr}
		c.Commit.Author = sig
		c.Commit.Committer = sig
		out = append(out, c)
	}
	a.writeJSON(w, 200, out)
}

// restGitRefs handles /repos/{o}/{r}/git/refs[/...].
func (a *API) restGitRefs(w *ResponseWriter, r *Request, owner, name string, rest []string) {
	repo, ok := a.loadRepo(w, owner, name)
	if !ok {
		return
	}
	method := r.Method
	if len(rest) == 0 {
		switch method {
		case "POST":
			a.restCreateRef(w, r, repo)
		case "GET":
			refs := a.store.ListRefs(repo)
			keys := make([]string, 0, len(refs))
			for k := range refs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out := make([]map[string]any, 0, len(keys))
			for _, k := range keys {
				out = append(out, map[string]any{"ref": k, "object": map[string]string{"sha": refs[k]}})
			}
			a.writeJSON(w, 200, out)
		default:
			a.writeErr(w, 404, "Not Found")
		}
		return
	}
	if method != "GET" {
		a.writeErr(w, 404, "Not Found")
		return
	}
	// GET /git/refs/heads/{branch} (branch may itself hold slashes).
	ref := "refs/" + strings.Join(rest, "/")
	sha, ok := a.store.GetRef(repo, ref)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeJSON(w, 200, map[string]any{"ref": ref, "object": map[string]string{"sha": sha}})
}

func (a *API) restCreateRef(w *ResponseWriter, r *Request, repo *Repo) {
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	ref := bodyString(m, "ref")
	sha := bodyString(m, "sha")
	if ref == "" || sha == "" || !strings.HasPrefix(ref, "refs/heads/") {
		a.writeErr(w, 422, "Invalid ref")
		return
	}
	if _, exists := a.store.GetRef(repo, ref); exists {
		a.writeErr(w, 422, "Reference already exists")
		return
	}
	if _, ok := a.store.GetObject(repo, sha); !ok {
		a.writeErr(w, 422, "Object does not exist")
		return
	}
	a.store.SetRef(repo, ref, sha)
	a.writeJSON(w, 201, map[string]any{"ref": ref, "object": map[string]string{"sha": sha}})
}

// ---------------------------------------------------------------------------
// Git object helpers (tree / commit parsing over the Store)

type treeEntry struct {
	mode string
	name string
	sha  string
	dir  bool
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			break
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+21 {
			break
		}
		name := string(data[:nul])
		sha := hex.EncodeToString(data[nul+1 : nul+21])
		data = data[nul+21:]
		out = append(out, treeEntry{mode: mode, name: name, sha: sha, dir: mode == "40000" || mode == "040000"})
	}
	return out
}

func (a *API) readTree(repo *Repo, sha string) ([]treeEntry, bool) {
	obj, ok := a.store.GetObject(repo, sha)
	if !ok || obj.Type != "tree" {
		return nil, false
	}
	return parseTree(obj.Data), true
}

// treeMap returns path -> blob sha for every file under treeSHA.
func (a *API) treeMap(repo *Repo, treeSHA string) map[string]string {
	out := map[string]string{}
	a.treeMapInto(repo, treeSHA, "", out)
	return out
}

func (a *API) treeMapInto(repo *Repo, treeSHA, prefix string, out map[string]string) {
	entries, ok := a.readTree(repo, treeSHA)
	if !ok {
		return
	}
	for _, e := range entries {
		if e.dir {
			a.treeMapInto(repo, e.sha, prefix+e.name+"/", out)
		} else {
			out[prefix+e.name] = e.sha
		}
	}
}

// resolveRef turns a branch name, full ref or commit sha into a commit sha.
func (a *API) resolveRef(repo *Repo, ref string) (string, bool) {
	if ref == "" {
		ref = repo.DefaultBranch
	}
	if sha, ok := a.store.GetRef(repo, "refs/heads/"+ref); ok {
		return sha, true
	}
	if sha, ok := a.store.GetRef(repo, ref); ok {
		return sha, true
	}
	if len(ref) == 40 {
		if obj, ok := a.store.GetObject(repo, ref); ok && obj.Type == "commit" {
			return ref, true
		}
	}
	return "", false
}

// commitTreeSHA returns the tree sha a commit points at.
func (a *API) commitTreeSHA(repo *Repo, commitSHA string) (string, bool) {
	obj, ok := a.store.GetObject(repo, commitSHA)
	if !ok || obj.Type != "commit" {
		return "", false
	}
	for _, line := range strings.Split(string(obj.Data), "\n") {
		if strings.HasPrefix(line, "tree ") {
			return strings.TrimSpace(line[5:]), true
		}
		if line == "" {
			break
		}
	}
	return "", false
}

func (a *API) commitParents(repo *Repo, commitSHA string) []string {
	obj, ok := a.store.GetObject(repo, commitSHA)
	if !ok || obj.Type != "commit" {
		return nil
	}
	var parents []string
	for _, line := range strings.Split(string(obj.Data), "\n") {
		if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		}
		if line == "" {
			break
		}
	}
	return parents
}

func (a *API) commitMessage(repo *Repo, commitSHA string) string {
	obj, ok := a.store.GetObject(repo, commitSHA)
	if !ok || obj.Type != "commit" {
		return ""
	}
	data := string(obj.Data)
	if i := strings.Index(data, "\n\n"); i >= 0 {
		return strings.TrimRight(data[i+2:], "\n")
	}
	return ""
}

// refTreeMap returns the path -> blob sha map at a ref (branch/sha), plus
// whether the ref exists at all.
func (a *API) refTreeMap(repo *Repo, ref string) (map[string]string, bool) {
	tip, ok := a.resolveRef(repo, ref)
	if !ok {
		return nil, false
	}
	treeSHA, ok := a.commitTreeSHA(repo, tip)
	if !ok {
		return nil, false
	}
	return a.treeMap(repo, treeSHA), true
}

// ---------------------------------------------------------------------------
// Contents

func (a *API) restContents(w *ResponseWriter, r *Request, owner, name string, rest []string) {
	repo, ok := a.loadRepo(w, owner, name)
	if !ok {
		return
	}
	path := strings.Join(rest, "/")
	switch r.Method {
	case "GET":
		a.restGetContents(w, r, repo, path)
	case "PUT":
		a.restPutContents(w, r, repo, path)
	case "DELETE":
		a.restDeleteContents(w, r, repo, path)
	default:
		a.writeErr(w, 404, "Not Found")
	}
}

func (a *API) restGetContents(w *ResponseWriter, r *Request, repo *Repo, path string) {
	q := queryOf(r)
	ref := q.Get("ref")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	tip, ok := a.resolveRef(repo, ref)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	treeSHA, ok := a.commitTreeSHA(repo, tip)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	files := a.treeMap(repo, treeSHA)
	if path == "" {
		a.writeJSON(w, 200, a.dirEntries(repo, treeSHA, ""))
		return
	}
	if blobSHA, isFile := files[path]; isFile {
		obj, ok := a.store.GetObject(repo, blobSHA)
		if !ok {
			a.writeErr(w, 404, "Not Found")
			return
		}
		base := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base = path[i+1:]
		}
		a.writeJSON(w, 200, map[string]any{
			"type": "file", "encoding": "base64", "size": len(obj.Data),
			"name": base, "path": path, "sha": blobSHA,
			"content": b64enc(obj.Data),
		})
		return
	}
	// Directory? List immediate children under path+"/".
	prefix := path + "/"
	found := false
	for p := range files {
		if strings.HasPrefix(p, prefix) {
			found = true
			break
		}
	}
	if !found {
		a.writeErr(w, 404, "Not Found")
		return
	}
	// Walk to the subtree for this directory to get real subtree SHAs.
	entries := a.dirEntriesAt(repo, treeSHA, path)
	a.writeJSON(w, 200, entries)
}

// dirEntries lists the root tree's entries.
func (a *API) dirEntries(repo *Repo, treeSHA, prefix string) []map[string]any {
	return a.dirEntriesAt(repo, treeSHA, prefix)
}

// dirEntriesAt lists the immediate children of dirPath ("" = root).
func (a *API) dirEntriesAt(repo *Repo, treeSHA, dirPath string) []map[string]any {
	cur := treeSHA
	if dirPath != "" {
		parts := strings.Split(dirPath, "/")
		ok := true
		for _, part := range parts {
			entries, good := a.readTree(repo, cur)
			if !good {
				ok = false
				break
			}
			next := ""
			for _, e := range entries {
				if e.dir && e.name == part {
					next = e.sha
					break
				}
			}
			if next == "" {
				ok = false
				break
			}
			cur = next
		}
		if !ok {
			return []map[string]any{}
		}
	}
	entries, ok := a.readTree(repo, cur)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		typ := "file"
		if e.dir {
			typ = "dir"
		}
		p := e.name
		if dirPath != "" {
			p = dirPath + "/" + e.name
		}
		out = append(out, map[string]any{
			"type": typ, "name": e.name, "path": p, "sha": e.sha,
		})
	}
	return out
}

func (a *API) restPutContents(w *ResponseWriter, r *Request, repo *Repo, path string) {
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	if path == "" {
		a.writeErr(w, 422, "Path is required")
		return
	}
	raw, err := b64dec(bodyString(m, "content"))
	if err != nil {
		a.writeErr(w, 422, "Content is not valid base64")
		return
	}
	branch := bodyString(m, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	message := bodyString(m, "message")
	if message == "" {
		message = "Update " + path
	}
	shaArg := bodyString(m, "sha")

	tip, hasTip := a.resolveRef(repo, branch)
	if !hasTip && branch != repo.DefaultBranch {
		a.writeErr(w, 422, "Branch does not exist")
		return
	}
	files := map[string]string{}
	if hasTip {
		treeSHA, ok := a.commitTreeSHA(repo, tip)
		if !ok {
			a.writeErr(w, 404, "Not Found")
			return
		}
		files = a.treeMap(repo, treeSHA)
	}
	_, existed := files[path]
	if shaArg != "" {
		cur, ok := files[path]
		if !ok || cur != shaArg {
			a.writeErr(w, 409, "Conflict: sha does not match the current blob")
			return
		}
	}
	blobSHA := a.store.PutObject(repo, "blob", raw)
	files[path] = blobSHA
	treeSHA := a.store.BuildTree(repo, files)
	parents := []string{}
	if hasTip {
		parents = []string{tip}
	}
	commitSHA := a.store.CommitTree(repo, treeSHA, message, a.viewer, parents)
	a.store.SetRef(repo, "refs/heads/"+branch, commitSHA)
	status := 201
	if existed {
		status = 200
	}
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	a.writeJSON(w, status, map[string]any{
		"content": map[string]any{
			"type": "file", "encoding": "base64", "size": len(raw),
			"name": base, "path": path, "sha": blobSHA,
		},
		"commit": map[string]string{"sha": commitSHA},
	})
}

func (a *API) restDeleteContents(w *ResponseWriter, r *Request, repo *Repo, path string) {
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	if path == "" {
		a.writeErr(w, 422, "Path is required")
		return
	}
	branch := bodyString(m, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	message := bodyString(m, "message")
	if message == "" {
		message = "Delete " + path
	}
	shaArg := bodyString(m, "sha")
	tip, hasTip := a.resolveRef(repo, branch)
	if !hasTip {
		a.writeErr(w, 404, "Not Found")
		return
	}
	treeSHA, ok := a.commitTreeSHA(repo, tip)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	files := a.treeMap(repo, treeSHA)
	cur, exists := files[path]
	if !exists {
		a.writeErr(w, 404, "Not Found")
		return
	}
	if shaArg != "" && shaArg != cur {
		a.writeErr(w, 409, "Conflict: sha does not match the current blob")
		return
	}
	delete(files, path)
	newTree := a.store.BuildTree(repo, files)
	commitSHA := a.store.CommitTree(repo, newTree, message, a.viewer, []string{tip})
	a.store.SetRef(repo, "refs/heads/"+branch, commitSHA)
	a.writeJSON(w, 200, map[string]any{"commit": map[string]string{"sha": commitSHA}})
}

// ---------------------------------------------------------------------------
// Labels

func (a *API) restCreateLabel(w *ResponseWriter, r *Request, owner, name string) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	labelName := bodyString(m, "name")
	if labelName == "" {
		a.writeErr(w, 422, "Label name is required")
		return
	}
	label, err := a.store.CreateLabel(owner, name, labelName)
	if err != nil {
		a.writeErr(w, 422, "Label could not be created")
		return
	}
	color := bodyString(m, "color")
	if color == "" {
		color = label.Color
	}
	if color == "" {
		color = "ededed"
	}
	desc := bodyString(m, "description")
	if desc == "" {
		desc = label.Description
	}
	a.mu.Lock()
	a.labelMeta[owner+"\x00"+name+"\x00"+labelName] = labelMeta{Color: color, Description: desc}
	a.mu.Unlock()
	a.writeJSON(w, 201, labelJSON{Name: label.Name, Color: color, Description: desc})
}

// ---------------------------------------------------------------------------
// Issues

func (a *API) restIssues(w *ResponseWriter, r *Request, owner, name string, rest []string) {
	method := r.Method
	if len(rest) == 0 {
		switch method {
		case "GET":
			a.restListIssues(w, r, owner, name)
		case "POST":
			a.restCreateIssue(w, r, owner, name)
		default:
			a.writeErr(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || num < 1 {
		a.writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch method {
		case "GET":
			a.restGetIssue(w, owner, name, num)
		case "PATCH":
			a.restPatchIssue(w, r, owner, name, num)
		default:
			a.writeErr(w, 404, "Not Found")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch method {
		case "GET":
			a.restListComments(w, owner, name, num)
		case "POST":
			a.restCreateComment(w, r, owner, name, num)
		default:
			a.writeErr(w, 404, "Not Found")
		}
		return
	}
	a.writeErr(w, 404, "Not Found")
}

func (a *API) restCreateIssue(w *ResponseWriter, r *Request, owner, name string) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	title := bodyString(m, "title")
	if title == "" {
		a.writeErr(w, 422, "Title is required")
		return
	}
	labels := bodyStringList(m, "labels")
	iss, err := a.store.CreateIssue(owner, name, title, bodyString(m, "body"), labels, a.viewer)
	if err != nil {
		a.writeErr(w, 422, "Validation Failed: a label does not exist")
		return
	}
	a.writeJSON(w, 201, a.issueToJSON(iss))
}

func (a *API) restListIssues(w *ResponseWriter, r *Request, owner, name string) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	q := queryOf(r)
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	// The Store filters by state; "" means all. Map GitHub's default (open).
	storeState := state
	if state != "open" && state != "closed" && state != "all" {
		storeState = "open"
	}
	if storeState == "all" {
		storeState = ""
	}
	issues := a.store.ListIssues(owner, name, storeState)
	// All created/updated timestamps are the fixed instant, so creation order
	// is the issue number: sort=created asc means number asc, desc the reverse.
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	if q.Get("sort") == "created" && q.Get("direction") == "desc" {
		for i, j := 0, len(issues)-1; i < j; i, j = i+1, j-1 {
			issues[i], issues[j] = issues[j], issues[i]
		}
	} else if q.Get("sort") == "created" && q.Get("direction") != "asc" {
		// No explicit direction: GitHub lists newest first.
		for i, j := 0, len(issues)-1; i < j; i, j = i+1, j-1 {
			issues[i], issues[j] = issues[j], issues[i]
		}
	}
	page, perPage := pageParams(q)
	items := pageSlice(issues, page, perPage)
	out := make([]issueJSON, 0, len(items))
	for _, iss := range items {
		out = append(out, a.issueToJSON(iss))
	}
	if next := a.linkNext(r, q, page, perPage, len(issues)); next != "" {
		w.Header("Link", next)
	}
	a.writeJSON(w, 200, out)
}

func (a *API) restGetIssue(w *ResponseWriter, owner, name string, num int64) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	iss, ok := a.store.GetIssue(owner, name, num)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeJSON(w, 200, a.issueToJSON(iss))
}

func (a *API) restPatchIssue(w *ResponseWriter, r *Request, owner, name string, num int64) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	state := bodyString(m, "state")
	if state != "" && state != "open" && state != "closed" {
		a.writeErr(w, 422, "Invalid state")
		return
	}
	if state != "" {
		iss, ok := a.store.UpdateIssueState(owner, name, num, state)
		if !ok {
			a.writeErr(w, 404, "Not Found")
			return
		}
		a.writeJSON(w, 200, a.issueToJSON(iss))
		return
	}
	iss, ok := a.store.GetIssue(owner, name, num)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeJSON(w, 200, a.issueToJSON(iss))
}

func (a *API) restCreateComment(w *ResponseWriter, r *Request, owner, name string, num int64) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	body := bodyString(m, "body")
	if body == "" {
		a.writeErr(w, 422, "Body is required")
		return
	}
	id, ok := a.addComment(owner, name, num, body, a.viewer)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	recs := a.listComments(owner, name, num)
	var rec *commentRec
	for _, c := range recs {
		if c.ID == id {
			rec = c
		}
	}
	if rec == nil {
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeJSON(w, 201, a.commentToJSON(owner, name, num, rec))
}

func (a *API) restListComments(w *ResponseWriter, owner, name string, num int64) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	if _, ok := a.store.GetIssue(owner, name, num); !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	recs := a.listComments(owner, name, num)
	out := make([]commentJSON, 0, len(recs))
	for _, c := range recs {
		out = append(out, a.commentToJSON(owner, name, num, c))
	}
	a.writeJSON(w, 200, out)
}

// ---------------------------------------------------------------------------
// Pull requests

func (a *API) restPulls(w *ResponseWriter, r *Request, owner, name string, rest []string) {
	method := r.Method
	if len(rest) == 0 {
		switch method {
		case "GET":
			a.restListPulls(w, r, owner, name)
		case "POST":
			a.restCreatePull(w, r, owner, name)
		default:
			a.writeErr(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || num < 1 {
		a.writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 1 {
		if method == "GET" {
			a.restGetPull(w, owner, name, num)
			return
		}
		a.writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" {
		if method == "GET" {
			a.restPullFiles(w, owner, name, num)
			return
		}
		a.writeErr(w, 404, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "merge" {
		if method == "PUT" {
			a.restMergePull(w, r, owner, name, num)
			return
		}
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeErr(w, 404, "Not Found")
}

func shortRef(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func (a *API) restCreatePull(w *ResponseWriter, r *Request, owner, name string) {
	repo, ok := a.loadRepo(w, owner, name)
	if !ok {
		return
	}
	m, ok := a.requireBody(w, r)
	if !ok {
		return
	}
	title := bodyString(m, "title")
	head := shortRef(bodyString(m, "head"))
	base := shortRef(bodyString(m, "base"))
	if title == "" || head == "" || base == "" {
		a.writeErr(w, 422, "Title, head and base are required")
		return
	}
	if _, ok := a.store.GetRef(repo, "refs/heads/"+head); !ok {
		a.writeErr(w, 422, "Head branch does not exist")
		return
	}
	if _, ok := a.store.GetRef(repo, "refs/heads/"+base); !ok {
		a.writeErr(w, 422, "Base branch does not exist")
		return
	}
	pull, err := a.store.CreatePull(owner, name, title, bodyString(m, "body"), head, base, a.viewer)
	if err != nil {
		a.writeErr(w, 422, "Pull request could not be created")
		return
	}
	a.writeJSON(w, 201, a.pullToJSON(pull))
}

func (a *API) restListPulls(w *ResponseWriter, r *Request, owner, name string) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	q := queryOf(r)
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	storeState := state
	if state != "open" && state != "closed" && state != "all" {
		storeState = "open"
	}
	if storeState == "all" {
		storeState = ""
	}
	pulls := a.store.ListPulls(owner, name, storeState)
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
	page, perPage := pageParams(q)
	items := pageSlice(pulls, page, perPage)
	out := make([]pullJSON, 0, len(items))
	for _, p := range items {
		out = append(out, a.pullToJSON(p))
	}
	if next := a.linkNext(r, q, page, perPage, len(pulls)); next != "" {
		w.Header("Link", next)
	}
	a.writeJSON(w, 200, out)
}

func (a *API) restGetPull(w *ResponseWriter, owner, name string, num int64) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	pull, ok := a.store.GetPull(owner, name, num)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeJSON(w, 200, a.pullToJSON(pull))
}

// pullChangedFiles diffs the base and head commit trees of a pull request.
// It falls back to the Pull's recorded file list when a tree is unreadable.
func (a *API) pullChangedFiles(repo *Repo, pull *Pull) []map[string]any {
	baseTree, okB := a.commitTreeSHA(repo, pull.BaseSHA)
	headTree, okH := a.commitTreeSHA(repo, pull.HeadSHA)
	if !okB || !okH {
		out := make([]map[string]any, 0, len(pull.Files))
		for _, f := range pull.Files {
			out = append(out, map[string]any{
				"filename": f, "status": "modified",
				"additions": 1, "deletions": 0, "changes": 1,
			})
		}
		return out
	}
	baseFiles := a.treeMap(repo, baseTree)
	headFiles := a.treeMap(repo, headTree)
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, p := range names {
		bSha, inBase := baseFiles[p]
		hSha, inHead := headFiles[p]
		if inBase && inHead && bSha == hSha {
			continue
		}
		status := "modified"
		additions, deletions := 0, 0
		if !inBase {
			status = "added"
		} else if !inHead {
			status = "removed"
		}
		if inHead {
			if obj, ok := a.store.GetObject(repo, hSha); ok {
				additions = countLines(obj.Data)
			}
		}
		if inBase {
			if obj, ok := a.store.GetObject(repo, bSha); ok {
				deletions = countLines(obj.Data)
			}
		}
		if status == "added" {
			deletions = 0
		}
		if status == "removed" {
			additions = 0
		}
		if additions < 1 && status != "removed" {
			additions = 1
		}
		out = append(out, map[string]any{
			"filename": p, "status": status,
			"additions": additions, "deletions": deletions,
			"changes": additions + deletions,
		})
	}
	return out
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

func (a *API) restPullFiles(w *ResponseWriter, owner, name string, num int64) {
	repo, ok := a.loadRepo(w, owner, name)
	if !ok {
		return
	}
	pull, ok := a.store.GetPull(owner, name, num)
	if !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	a.writeJSON(w, 200, a.pullChangedFiles(repo, pull))
}

func (a *API) restMergePull(w *ResponseWriter, r *Request, owner, name string, num int64) {
	if _, ok := a.loadRepo(w, owner, name); !ok {
		return
	}
	if _, ok := a.requireBody(w, r); !ok {
		return
	}
	if _, ok := a.store.GetPull(owner, name, num); !ok {
		a.writeErr(w, 404, "Not Found")
		return
	}
	merged, err := a.store.MergePull(owner, name, num)
	if err != nil {
		a.writeErr(w, 405, "Merge conflict: pull request is conflicted")
		return
	}
	a.writeJSON(w, 200, map[string]any{
		"sha": merged.MergeSHA, "merged": true,
		"message": "Pull request successfully merged",
	})
}
