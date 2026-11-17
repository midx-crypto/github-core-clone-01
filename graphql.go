// graphql.go -- GraphQL v4: hand-written parser/executor for exactly the
// documents gh and github-mcp-server send (see kit checks-graphql.ts), plus
// the required introspection types. No GraphQL library is used.
//
// Supported: query/mutation operations, $variables, aliases, inline and
// named fragments, arguments as literals/variables/lists/input objects,
// operation batching (a JSON array body answers an array, in order).
// An unknown field is an HTTP 200 with an errors array naming it.
// An unknown repository is data.repository null with no errors.

package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lexer

type gqlTokKind int

const (
	tEOF gqlTokKind = iota
	tLBrace
	tRBrace
	tLParen
	tRParen
	tLBracket
	tRBracket
	tColon
	tDollar
	tBang
	tEq
	tAt
	tSpread // ...
	tName
	tString
	tInt
	tFloat
)

type gqlTok struct {
	kind gqlTokKind
	text string
}

func gqlLex(src string) ([]gqlTok, bool) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{':
			toks = append(toks, gqlTok{tLBrace, "{"})
			i++
		case c == '}':
			toks = append(toks, gqlTok{tRBrace, "}"})
			i++
		case c == '(':
			toks = append(toks, gqlTok{tLParen, "("})
			i++
		case c == ')':
			toks = append(toks, gqlTok{tRParen, ")"})
			i++
		case c == '[':
			toks = append(toks, gqlTok{tLBracket, "["})
			i++
		case c == ']':
			toks = append(toks, gqlTok{tRBracket, "]"})
			i++
		case c == ':':
			toks = append(toks, gqlTok{tColon, ":"})
			i++
		case c == '$':
			toks = append(toks, gqlTok{tDollar, "$"})
			i++
		case c == '!':
			toks = append(toks, gqlTok{tBang, "!"})
			i++
		case c == '=':
			toks = append(toks, gqlTok{tEq, "="})
			i++
		case c == '@':
			toks = append(toks, gqlTok{tAt, "@"})
			i++
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{tSpread, "..."})
			i += 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			ok := false
			for j < len(src) {
				if src[j] == '\\' && j+1 < len(src) {
					switch src[j+1] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case 'u':
						if j+5 < len(src) {
							if n, err := strconv.ParseInt(src[j+2:j+6], 16, 32); err == nil {
								sb.WriteRune(rune(n))
								j += 4
							}
						}
					default:
						sb.WriteByte(src[j+1])
					}
					j += 2
					continue
				}
				if src[j] == '"' {
					ok = true
					j++
					break
				}
				sb.WriteByte(src[j])
				j++
			}
			if !ok {
				return nil, false
			}
			toks = append(toks, gqlTok{tString, sb.String()})
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < len(src) && src[j] == '.' {
				isFloat = true
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				isFloat = true
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if isFloat {
				toks = append(toks, gqlTok{tFloat, src[i:j]})
			} else {
				toks = append(toks, gqlTok{tInt, src[i:j]})
			}
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(src) {
				d := src[j]
				if d == '_' || (d >= 'a' && d <= 'z') || (d >= 'A' && d <= 'Z') || (d >= '0' && d <= '9') {
					j++
				} else {
					break
				}
			}
			toks = append(toks, gqlTok{tName, src[i:j]})
			i = j
		default:
			return nil, false
		}
	}
	toks = append(toks, gqlTok{tEOF, ""})
	return toks, true
}

// ---------------------------------------------------------------------------
// AST

type gqlValue struct {
	kind string // var, string, int, float, bool, null, enum, list, object
	str  string
	num  string
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlArg struct {
	name  string
	value *gqlValue
}

type gqlField struct {
	alias  string
	name   string
	args   []gqlArg
	sel    []*gqlField
	isFrag bool
	fragOn string // type condition for ... on T ("" = none)
	frag   string // named fragment name for ...Name
}

type gqlOp struct {
	opType string // query | mutation
	name   string
	sel    []*gqlField
}

type gqlDoc struct {
	ops   []*gqlOp
	frags map[string]*gqlField // named fragments: name -> pseudo-field holding sel + type condition
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) eat(k gqlTokKind) bool {
	if p.toks[p.pos].kind == k {
		p.pos++
		return true
	}
	return false
}
func (p *gqlParser) expect(k gqlTokKind) bool { return p.eat(k) }

func gqlParse(src string) (*gqlDoc, bool) {
	toks, ok := gqlLex(src)
	if !ok {
		return nil, false
	}
	p := &gqlParser{toks: toks}
	doc := &gqlDoc{frags: map[string]*gqlField{}}
	for p.peek().kind != tEOF {
		if p.peek().kind == tSpread {
			// Fragment definition: ... handled below via name check.
			return nil, false
		}
		if p.peek().kind == tName && p.peek().text == "fragment" {
			p.next()
			nameTok := p.next()
			if nameTok.kind != tName {
				return nil, false
			}
			if !p.eat(tName) || p.toks[p.pos-1].text != "on" {
				return nil, false
			}
			typeTok := p.next()
			if typeTok.kind != tName {
				return nil, false
			}
			sel, ok := p.parseSelectionSet()
			if !ok {
				return nil, false
			}
			doc.frags[nameTok.text] = &gqlField{isFrag: true, fragOn: typeTok.text, sel: sel}
			continue
		}
		op, ok := p.parseOperation()
		if !ok {
			return nil, false
		}
		doc.ops = append(doc.ops, op)
	}
	if len(doc.ops) == 0 {
		return nil, false
	}
	return doc, true
}

func (p *gqlParser) parseOperation() (*gqlOp, bool) {
	op := &gqlOp{opType: "query"}
	if p.peek().kind == tName && (p.peek().text == "query" || p.peek().text == "mutation" || p.peek().text == "subscription") {
		op.opType = p.next().text
		if op.opType == "subscription" {
			return nil, false
		}
		if p.peek().kind == tName {
			op.name = p.next().text
		}
	}
	// Variable definitions: ($x: Type = default, ...)
	if p.peek().kind == tLParen {
		if !p.skipParens() {
			return nil, false
		}
	}
	sel, ok := p.parseSelectionSet()
	if !ok {
		return nil, false
	}
	op.sel = sel
	return op, true
}

// skipParens consumes a balanced (...) group (variable definitions).
func (p *gqlParser) skipParens() bool {
	depth := 0
	for {
		t := p.next()
		if t.kind == tEOF {
			return false
		}
		switch t.kind {
		case tLParen, tLBracket:
			depth++
		case tRParen, tRBracket:
			depth--
			if depth == 0 {
				return true
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, bool) {
	if !p.eat(tLBrace) {
		return nil, false
	}
	var out []*gqlField
	for p.peek().kind != tRBrace {
		if p.peek().kind == tEOF {
			return nil, false
		}
		f, ok := p.parseSelection()
		if !ok {
			return nil, false
		}
		out = append(out, f)
	}
	p.next() // }
	return out, true
}

func (p *gqlParser) parseSelection() (*gqlField, bool) {
	if p.peek().kind == tSpread {
		p.next()
		if p.peek().kind == tName && p.peek().text == "on" {
			p.next()
			typeTok := p.next()
			if typeTok.kind != tName {
				return nil, false
			}
			sel, ok := p.parseSelectionSet()
			if !ok {
				return nil, false
			}
			return &gqlField{isFrag: true, fragOn: typeTok.text, sel: sel}, true
		}
		nameTok := p.next()
		if nameTok.kind != tName {
			return nil, false
		}
		return &gqlField{isFrag: true, frag: nameTok.text}, true
	}
	return p.parseField()
}

func (p *gqlParser) parseField() (*gqlField, bool) {
	nameTok := p.next()
	if nameTok.kind != tName {
		return nil, false
	}
	f := &gqlField{name: nameTok.text}
	if p.peek().kind == tColon {
		p.next()
		realTok := p.next()
		if realTok.kind != tName {
			return nil, false
		}
		f.alias = f.name
		f.name = realTok.text
	}
	// Directives @name(...) -- skipped, none of the suite's documents use them.
	for p.peek().kind == tAt {
		p.next()
		if p.peek().kind == tName {
			p.next()
		}
		if p.peek().kind == tLParen {
			if !p.skipParens() {
				return nil, false
			}
		}
	}
	if p.peek().kind == tLParen {
		p.next()
		for p.peek().kind != tRParen {
			if p.peek().kind == tEOF {
				return nil, false
			}
			argName := p.next()
			if argName.kind != tName {
				return nil, false
			}
			if !p.eat(tColon) {
				return nil, false
			}
			v, ok := p.parseValue()
			if !ok {
				return nil, false
			}
			f.args = append(f.args, gqlArg{name: argName.text, value: v})
		}
		p.next() // )
	}
	if p.peek().kind == tLBrace {
		sel, ok := p.parseSelectionSet()
		if !ok {
			return nil, false
		}
		f.sel = sel
	}
	return f, true
}

func (p *gqlParser) parseValue() (*gqlValue, bool) {
	t := p.next()
	switch t.kind {
	case tDollar:
		nameTok := p.next()
		if nameTok.kind != tName {
			return nil, false
		}
		return &gqlValue{kind: "var", str: nameTok.text}, true
	case tString:
		return &gqlValue{kind: "string", str: t.text}, true
	case tInt:
		return &gqlValue{kind: "int", num: t.text}, true
	case tFloat:
		return &gqlValue{kind: "float", num: t.text}, true
	case tLBracket:
		v := &gqlValue{kind: "list"}
		for p.peek().kind != tRBracket {
			if p.peek().kind == tEOF {
				return nil, false
			}
			item, ok := p.parseValue()
			if !ok {
				return nil, false
			}
			v.list = append(v.list, item)
		}
		p.next()
		return v, true
	case tLBrace:
		v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
		for p.peek().kind != tRBrace {
			if p.peek().kind == tEOF {
				return nil, false
			}
			nameTok := p.next()
			if nameTok.kind != tName {
				return nil, false
			}
			if !p.eat(tColon) {
				return nil, false
			}
			item, ok := p.parseValue()
			if !ok {
				return nil, false
			}
			v.obj[nameTok.text] = item
		}
		p.next()
		return v, true
	case tName:
		switch t.text {
		case "true":
			return &gqlValue{kind: "bool", str: "true"}, true
		case "false":
			return &gqlValue{kind: "bool", str: "false"}, true
		case "null":
			return &gqlValue{kind: "null"}, true
		default:
			return &gqlValue{kind: "enum", str: t.text}, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Executor

type gqlCtx struct {
	api    *API
	vars   map[string]any
	doc    *gqlDoc
	errors []map[string]any
	path   []string
}

func (c *gqlCtx) addErr(msg string) {
	e := map[string]any{"message": msg}
	if len(c.path) > 0 {
		p := make([]any, len(c.path))
		for i, s := range c.path {
			p[i] = s
		}
		e["path"] = p
	}
	c.errors = append(c.errors, e)
}

// gqlNode is one resolved value: a single object (val), a list (list), or a
// scalar (val with kind String/Int/Bool/Float).
type gqlNode struct {
	kind string
	val  any
	list []gqlNode
}

func scalarNode(kind string, v any) gqlNode { return gqlNode{kind: kind, val: v} }

// registerGraphQL wires the GraphQL v4 surface into the router. The shared
// API instance (see shared in rest.go) keeps comments and overlays in sync
// with the REST and MCP surfaces.
func registerGraphQL(rt *Router, store *Store) {
	api := shared(store)
	rt.Handle("POST", "/api/graphql", func(w *ResponseWriter, r *Request) {
		api.graphql(w, r)
	})
}

func (a *API) graphql(w *ResponseWriter, r *Request) {
	if r.Method != "POST" {
		a.writeErr(w, 405, "Method Not Allowed")
		return
	}
	if !a.requireAuth(w, r) {
		return
	}
	var raw any
	if err := json.Unmarshal(r.Body, &raw); err != nil {
		a.writeJSON(w, 400, map[string]any{"errors": []map[string]any{{"message": "Request body is not valid JSON"}}})
		return
	}
	switch body := raw.(type) {
	case []any:
		// Operation batching: one result per operation, in order.
		out := make([]any, 0, len(body))
		for _, entry := range body {
			out = append(out, a.execGraphQLOp(entry))
		}
		a.writeJSON(w, 200, out)
	case map[string]any:
		a.writeJSON(w, 200, a.execGraphQLOp(body))
	default:
		a.writeJSON(w, 400, map[string]any{"errors": []map[string]any{{"message": "Request body must be a JSON object or array"}}})
	}
}

func (a *API) execGraphQLOp(body any) map[string]any {
	op, ok := body.(map[string]any)
	if !ok {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "Operation must be a JSON object"}}}
	}
	query, _ := op["query"].(string)
	vars := map[string]any{}
	if v, ok := op["variables"].(map[string]any); ok {
		vars = v
	}
	doc, ok := gqlParse(query)
	if !ok {
		return map[string]any{"data": nil, "errors": []map[string]any{{"message": "Could not parse the query document"}}}
	}
	ctx := &gqlCtx{api: a, vars: vars, doc: doc}
	// Pick the operation: by operationName, else the first (only) one.
	chosen := doc.ops[0]
	if name, _ := op["operationName"].(string); name != "" {
		for _, o := range doc.ops {
			if o.name == name {
				chosen = o
				break
			}
		}
	}
	rootKind := "Query"
	if chosen.opType == "mutation" {
		rootKind = "Mutation"
	}
	data := a.execSel(ctx, chosen.sel, gqlNode{kind: rootKind}, nil)
	res := map[string]any{"data": data}
	if len(ctx.errors) > 0 {
		res["errors"] = ctx.errors
	}
	return res
}

// evalValue turns a parsed literal into a Go value, substituting $variables.
func (c *gqlCtx) evalValue(v *gqlValue) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "var":
		return c.vars[v.str]
	case "string", "enum":
		return v.str
	case "int":
		n, _ := strconv.ParseInt(v.num, 10, 64)
		return n
	case "float":
		f, _ := strconv.ParseFloat(v.num, 64)
		return f
	case "bool":
		return v.str == "true"
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, c.evalValue(item))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, item := range v.obj {
			out[k] = c.evalValue(item)
		}
		return out
	}
	return nil
}

func (c *gqlCtx) argMap(f *gqlField) map[string]any {
	out := map[string]any{}
	for _, arg := range f.args {
		out[arg.name] = c.evalValue(arg.value)
	}
	return out
}

func argStr(args map[string]any, name string) string {
	if s, ok := args[name].(string); ok {
		return s
	}
	return ""
}

func argInt(args map[string]any, name string) int64 {
	switch v := args[name].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}

// ---------------------------------------------------------------------------
// Field tables: which fields each type is known to have. Anything else is an
// error naming the field (HTTP 200 with an errors array).

var gqlKnownFields = map[string]map[string]bool{
	"Query":    {"viewer": true, "repository": true, "node": true, "__type": true},
	"Mutation": {"createIssue": true, "closeIssue": true, "createPullRequest": true, "mergePullRequest": true, "addComment": true},
	"Repository": {"id": true, "databaseId": true, "name": true, "nameWithOwner": true, "owner": true,
		"defaultBranchRef": true, "viewerPermission": true, "hasIssuesEnabled": true,
		"issue": true, "issues": true, "pullRequests": true, "pullRequest": true,
		"description": true, "private": true, "url": true, "createdAt": true, "updatedAt": true},
	"Issue": {"id": true, "number": true, "title": true, "state": true, "body": true,
		"url": true, "createdAt": true, "updatedAt": true, "author": true, "repository": true},
	"PullRequest": {"id": true, "number": true, "title": true, "body": true, "state": true,
		"merged": true, "mergeable": true, "headRefName": true, "baseRefName": true,
		"files": true, "author": true},
	"User":                  {"login": true, "id": true},
	"Ref":                   {"name": true},
	"IssueConnection":       {"nodes": true, "pageInfo": true, "totalCount": true},
	"PullRequestConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"ChangedFileConnection": {"nodes": true, "pageInfo": true, "totalCount": true},
	"PageInfo":              {"hasNextPage": true, "hasPreviousPage": true, "endCursor": true, "startCursor": true},
	"ChangedFile":           {"path": true, "additions": true, "deletions": true, "changeType": true},
	"CommentEdge":           {"node": true, "cursor": true},
	"IssueComment":          {"id": true, "body": true, "author": true},
	"__Type":                {"name": true, "kind": true, "description": true, "fields": true, "enumValues": true},
	"__Field":               {"name": true, "description": true},
	"__EnumValue":           {"name": true, "description": true},
	"Node":                  {"id": true},
	// Mutation payload objects: the fields each mutation's payload can carry.
	"MutationPayload": {"issue": true, "pullRequest": true, "commentEdge": true, "clientMutationId": true},
}

// gqlTypeInfo describes one introspected type.
type gqlTypeInfo struct {
	name   string
	fields []string
	enums  []string
}

var gqlTypes = map[string]*gqlTypeInfo{
	"Issue": {
		name:   "Issue",
		fields: []string{"id", "number", "title", "state", "body", "url", "createdAt", "updatedAt", "author", "repository"},
	},
	"PullRequest": {
		name:   "PullRequest",
		fields: []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "author"},
	},
	"Repository": {
		name:   "Repository",
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt", "updatedAt"},
	},
	"SearchType": {
		name:  "SearchType",
		enums: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "MILESTONE", "COMMIT"},
	},
	"User": {
		name:   "User",
		fields: []string{"login", "id"},
	},
	"Query": {
		name:   "Query",
		fields: []string{"viewer", "repository", "node"},
	},
}

// execSel resolves a selection set against one node.
func (a *API) execSel(ctx *gqlCtx, fields []*gqlField, node gqlNode, path []string) map[string]any {
	// A null object resolves to null, not an empty object (except the roots,
	// whose val is legitimately nil).
	if node.val == nil && node.list == nil && node.kind != "Query" && node.kind != "Mutation" {
		return nil
	}
	out := map[string]any{}
	for _, f := range fields {
		if f.isFrag {
			if f.frag != "" {
				def, ok := ctx.doc.frags[f.frag]
				if !ok {
					ctx.addErr("Unknown fragment \"" + f.frag + "\".")
					continue
				}
				if def.fragOn != "" && def.fragOn != node.kind {
					continue
				}
				for k, v := range a.execSel(ctx, def.sel, node, path) {
					out[k] = v
				}
				continue
			}
			if f.fragOn != "" && f.fragOn != node.kind {
				continue
			}
			for k, v := range a.execSel(ctx, f.sel, node, path) {
				out[k] = v
			}
			continue
		}
		if !gqlKnownFields[node.kind][f.name] {
			saved := ctx.path
			ctx.path = append(append([]string{}, path...), f.aliasOrName())
			ctx.addErr("Cannot query field \"" + f.name + "\" on type \"" + node.kind + "\".")
			ctx.path = saved
			continue
		}
		key := f.aliasOrName()
		fpath := append(append([]string{}, path...), key)
		child := a.resolveField(ctx, node, f)
		if child == nil {
			out[key] = nil
			continue
		}
		if len(f.sel) == 0 {
			out[key] = child.val
			continue
		}
		if child.list != nil {
			arr := make([]any, 0, len(child.list))
			for _, elem := range child.list {
				arr = append(arr, a.execSel(ctx, f.sel, elem, fpath))
			}
			out[key] = arr
			continue
		}
		if child.val == nil {
			out[key] = nil
			continue
		}
		out[key] = a.execSel(ctx, f.sel, *child, fpath)
	}
	return out
}

func (f *gqlField) aliasOrName() string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

// ---------------------------------------------------------------------------
// Resolvers

type gqlPageInfo struct {
	hasNext   bool
	endCursor string
}

type gqlIssueConn struct {
	nodes []gqlNode
	page  gqlPageInfo
	total int
}

type gqlPullConn struct {
	nodes []gqlNode
	page  gqlPageInfo
	total int
}

type gqlFileConn struct {
	nodes []gqlNode
	page  gqlPageInfo
	total int
}

type gqlFile struct {
	path      string
	additions int
	deletions int
}

type gqlCommentEdge struct {
	id   string
	body string
}

func (a *API) resolveField(ctx *gqlCtx, node gqlNode, f *gqlField) *gqlNode {
	args := ctx.argMap(f)
	switch node.kind {
	case "Query":
		return a.resolveQuery(ctx, f, args)
	case "Mutation":
		return a.resolveMutation(ctx, f, args)
	case "MutationPayload":
		// Payload values are gqlNodes (typed, possibly nil-valued).
		m, _ := node.val.(map[string]any)
		if m == nil {
			return nil
		}
		v, ok := m[f.name]
		if !ok || v == nil {
			return nil
		}
		if n, ok := v.(gqlNode); ok {
			return &n
		}
		return nil
	case "Repository":
		return a.resolveRepository(ctx, node.val.(*Repo), f, args)
	case "Issue":
		return a.resolveIssue(ctx, node.val.(*Issue), f, args)
	case "PullRequest":
		return a.resolvePull(ctx, node.val.(*Pull), f, args)
	case "User":
		login := node.val.(string)
		switch f.name {
		case "login":
			return &gqlNode{kind: "String", val: login}
		case "id":
			return &gqlNode{kind: "Int", val: int64(viewerID)}
		}
	case "Ref":
		if f.name == "name" {
			return &gqlNode{kind: "String", val: node.val.(string)}
		}
	case "IssueConnection":
		c := node.val.(*gqlIssueConn)
		return a.resolveConn(f, c.nodes, c.page, c.total, "Issue")
	case "PullRequestConnection":
		c := node.val.(*gqlPullConn)
		return a.resolveConn(f, c.nodes, c.page, c.total, "PullRequest")
	case "ChangedFileConnection":
		c := node.val.(*gqlFileConn)
		return a.resolveConn(f, c.nodes, c.page, c.total, "ChangedFile")
	case "PageInfo":
		p := node.val.(gqlPageInfo)
		switch f.name {
		case "hasNextPage":
			return &gqlNode{kind: "Bool", val: p.hasNext}
		case "hasPreviousPage":
			return &gqlNode{kind: "Bool", val: false}
		case "endCursor":
			if p.endCursor == "" {
				return &gqlNode{kind: "String", val: nil}
			}
			return &gqlNode{kind: "String", val: p.endCursor}
		case "startCursor":
			return &gqlNode{kind: "String", val: nil}
		}
	case "ChangedFile":
		fl := node.val.(*gqlFile)
		switch f.name {
		case "path":
			return &gqlNode{kind: "String", val: fl.path}
		case "additions":
			return &gqlNode{kind: "Int", val: int64(fl.additions)}
		case "deletions":
			return &gqlNode{kind: "Int", val: int64(fl.deletions)}
		case "changeType":
			return &gqlNode{kind: "String", val: "MODIFIED"}
		}
	case "CommentEdge":
		e := node.val.(*gqlCommentEdge)
		switch f.name {
		case "node":
			return &gqlNode{kind: "IssueComment", val: e}
		case "cursor":
			return &gqlNode{kind: "String", val: ""}
		}
	case "IssueComment":
		e := node.val.(*gqlCommentEdge)
		switch f.name {
		case "id":
			return &gqlNode{kind: "String", val: e.id}
		case "body":
			return &gqlNode{kind: "String", val: e.body}
		case "author":
			return &gqlNode{kind: "User", val: ctx.api.viewer}
		}
	case "__Type":
		t := node.val.(*gqlTypeInfo)
		switch f.name {
		case "name":
			return &gqlNode{kind: "String", val: t.name}
		case "kind":
			if len(t.enums) > 0 {
				return &gqlNode{kind: "String", val: "ENUM"}
			}
			return &gqlNode{kind: "String", val: "OBJECT"}
		case "description":
			return &gqlNode{kind: "String", val: nil}
		case "fields":
			list := make([]gqlNode, 0, len(t.fields))
			for _, name := range t.fields {
				list = append(list, gqlNode{kind: "__Field", val: name})
			}
			return &gqlNode{kind: "__Field", list: list}
		case "enumValues":
			list := make([]gqlNode, 0, len(t.enums))
			for _, name := range t.enums {
				list = append(list, gqlNode{kind: "__EnumValue", val: name})
			}
			return &gqlNode{kind: "__EnumValue", list: list}
		}
	case "__Field", "__EnumValue":
		if f.name == "name" {
			return &gqlNode{kind: "String", val: node.val.(string)}
		}
		if f.name == "description" {
			return &gqlNode{kind: "String", val: nil}
		}
	case "Node":
		if f.name == "id" {
			return &gqlNode{kind: "String", val: node.val.(string)}
		}
	}
	return nil
}

func (a *API) resolveConn(f *gqlField, nodes []gqlNode, page gqlPageInfo, total int, _ string) *gqlNode {
	switch f.name {
	case "nodes":
		return &gqlNode{kind: nodesKind(nodes), list: nodes}
	case "pageInfo":
		return &gqlNode{kind: "PageInfo", val: page}
	case "totalCount":
		return &gqlNode{kind: "Int", val: int64(total)}
	}
	return nil
}

func nodesKind(nodes []gqlNode) string {
	if len(nodes) == 0 {
		return "Issue"
	}
	return nodes[0].kind
}

func (a *API) resolveQuery(ctx *gqlCtx, f *gqlField, args map[string]any) *gqlNode {
	switch f.name {
	case "viewer":
		return &gqlNode{kind: "User", val: ctx.api.viewer}
	case "repository":
		owner := argStr(args, "owner")
		name := argStr(args, "name")
		if owner != ctx.api.viewer {
			return &gqlNode{kind: "Repository", val: nil}
		}
		repo, ok := ctx.api.store.GetRepo(owner, name)
		if !ok {
			return &gqlNode{kind: "Repository", val: nil}
		}
		return &gqlNode{kind: "Repository", val: repo}
	case "node":
		id := argStr(args, "id")
		return a.resolveNode(ctx, id)
	case "__type":
		name := argStr(args, "name")
		t, ok := gqlTypes[name]
		if !ok {
			return &gqlNode{kind: "__Type", val: nil}
		}
		return &gqlNode{kind: "__Type", val: t}
	}
	return nil
}

// resolveNode answers node(id:) for the ids this package mints.
func (a *API) resolveNode(ctx *gqlCtx, id string) *gqlNode {
	kind, payload, ok := decodeNodeID(id)
	if !ok {
		return &gqlNode{kind: "Node", val: nil}
	}
	switch kind {
	case "Repo":
		n, _ := strconv.ParseInt(payload, 10, 64)
		for _, repo := range ctx.api.store.ListRepos(ctx.api.viewer) {
			if repo.ID == n {
				return &gqlNode{kind: "Repository", val: repo}
			}
		}
		return &gqlNode{kind: "Node", val: nil}
	case "Issue":
		owner, repo, num := splitNodeRef(payload)
		if iss, ok := ctx.api.store.GetIssue(owner, repo, num); ok {
			return &gqlNode{kind: "Issue", val: iss}
		}
		return &gqlNode{kind: "Node", val: nil}
	case "PullRequest":
		owner, repo, num := splitNodeRef(payload)
		if p, ok := ctx.api.store.GetPull(owner, repo, num); ok {
			return &gqlNode{kind: "PullRequest", val: p}
		}
		return &gqlNode{kind: "Node", val: nil}
	}
	return &gqlNode{kind: "Node", val: nil}
}

// splitNodeRef parses "owner/repo:number" into its parts.
func splitNodeRef(payload string) (owner, repo string, num int64) {
	i := strings.LastIndex(payload, ":")
	if i < 0 {
		return "", "", 0
	}
	n, _ := strconv.ParseInt(payload[i+1:], 10, 64)
	rest := payload[:i]
	j := strings.Index(rest, "/")
	if j < 0 {
		return rest, "", n
	}
	return rest[:j], rest[j+1:], n
}

func (a *API) resolveRepository(ctx *gqlCtx, repo *Repo, f *gqlField, args map[string]any) *gqlNode {
	switch f.name {
	case "id":
		return &gqlNode{kind: "String", val: nodeIDRepo(repo.ID)}
	case "databaseId":
		return &gqlNode{kind: "Int", val: repo.ID}
	case "name":
		return &gqlNode{kind: "String", val: repo.Name}
	case "nameWithOwner":
		return &gqlNode{kind: "String", val: repo.Owner + "/" + repo.Name}
	case "owner":
		return &gqlNode{kind: "User", val: repo.Owner}
	case "defaultBranchRef":
		if _, ok := ctx.api.store.GetRef(repo, "refs/heads/"+repo.DefaultBranch); ok {
			return &gqlNode{kind: "Ref", val: repo.DefaultBranch}
		}
		return &gqlNode{kind: "Ref", val: nil}
	case "viewerPermission":
		return &gqlNode{kind: "String", val: "ADMIN"}
	case "hasIssuesEnabled":
		return &gqlNode{kind: "Bool", val: true}
	case "description":
		return &gqlNode{kind: "String", val: repo.Description}
	case "private":
		return &gqlNode{kind: "Bool", val: ctx.api.repoToJSON(repo).Private}
	case "url":
		return &gqlNode{kind: "String", val: "https://github.com/" + repo.Owner + "/" + repo.Name}
	case "createdAt":
		return &gqlNode{kind: "String", val: repo.CreatedAt}
	case "updatedAt":
		return &gqlNode{kind: "String", val: repo.UpdatedAt}
	case "issue":
		num := argInt(args, "number")
		iss, ok := ctx.api.store.GetIssue(repo.Owner, repo.Name, num)
		if !ok {
			return &gqlNode{kind: "Issue", val: nil}
		}
		return &gqlNode{kind: "Issue", val: iss}
	case "issues":
		return &gqlNode{kind: "IssueConnection", val: a.issueConn(ctx, repo, args)}
	case "pullRequests":
		return &gqlNode{kind: "PullRequestConnection", val: a.pullConn(ctx, repo, args)}
	case "pullRequest":
		num := argInt(args, "number")
		p, ok := ctx.api.store.GetPull(repo.Owner, repo.Name, num)
		if !ok {
			return &gqlNode{kind: "PullRequest", val: nil}
		}
		return &gqlNode{kind: "PullRequest", val: p}
	}
	return nil
}

func (a *API) resolveIssue(ctx *gqlCtx, iss *Issue, f *gqlField, args map[string]any) *gqlNode {
	switch f.name {
	case "id":
		return &gqlNode{kind: "String", val: nodeIDIssue(iss.RepoOwner, iss.RepoName, iss.Number)}
	case "number":
		return &gqlNode{kind: "Int", val: iss.Number}
	case "title":
		return &gqlNode{kind: "String", val: iss.Title}
	case "state":
		return &gqlNode{kind: "String", val: strings.ToUpper(iss.State)}
	case "body":
		return &gqlNode{kind: "String", val: iss.Body}
	case "url":
		return &gqlNode{kind: "String", val: "https://github.com/" + iss.RepoOwner + "/" + iss.RepoName + "/issues/" + strconv.FormatInt(iss.Number, 10)}
	case "createdAt":
		return &gqlNode{kind: "String", val: iss.CreatedAt}
	case "updatedAt":
		return &gqlNode{kind: "String", val: iss.UpdatedAt}
	case "author":
		return &gqlNode{kind: "User", val: iss.User}
	case "repository":
		if repo, ok := ctx.api.store.GetRepo(iss.RepoOwner, iss.RepoName); ok {
			return &gqlNode{kind: "Repository", val: repo}
		}
		return &gqlNode{kind: "Repository", val: nil}
	}
	return nil
}

func pullState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	if p.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func (a *API) resolvePull(ctx *gqlCtx, p *Pull, f *gqlField, args map[string]any) *gqlNode {
	switch f.name {
	case "id":
		return &gqlNode{kind: "String", val: nodeIDPull(p.RepoOwner, p.RepoName, p.Number)}
	case "number":
		return &gqlNode{kind: "Int", val: p.Number}
	case "title":
		return &gqlNode{kind: "String", val: p.Title}
	case "body":
		return &gqlNode{kind: "String", val: p.Body}
	case "state":
		return &gqlNode{kind: "String", val: pullState(p)}
	case "merged":
		return &gqlNode{kind: "Bool", val: p.Merged}
	case "mergeable":
		if p.Mergeable {
			return &gqlNode{kind: "String", val: "MERGEABLE"}
		}
		return &gqlNode{kind: "String", val: "CONFLICTING"}
	case "headRefName":
		return &gqlNode{kind: "String", val: p.Head}
	case "baseRefName":
		return &gqlNode{kind: "String", val: p.Base}
	case "author":
		return &gqlNode{kind: "User", val: p.User}
	case "files":
		first := argInt(args, "first")
		repo := mustRepo(ctx, p)
		if repo == nil {
			return &gqlNode{kind: "ChangedFileConnection", val: &gqlFileConn{nodes: []gqlNode{}, total: 0}}
		}
		files := ctx.api.pullChangedFiles(repo, p)
		nodes := make([]gqlNode, 0, len(files))
		for _, fl := range files {
			if first > 0 && int64(len(nodes)) >= first {
				break
			}
			nodes = append(nodes, gqlNode{kind: "ChangedFile", val: &gqlFile{
				path:      fl["filename"].(string),
				additions: fl["additions"].(int),
				deletions: fl["deletions"].(int),
			}})
		}
		return &gqlNode{kind: "ChangedFileConnection", val: &gqlFileConn{nodes: nodes, total: len(files)}}
	}
	return nil
}

func mustRepo(ctx *gqlCtx, p *Pull) *Repo {
	repo, _ := ctx.api.store.GetRepo(p.RepoOwner, p.RepoName)
	return repo
}

// ---------------------------------------------------------------------------
// Connections (issues / pull requests) with cursor paging

func gqlCursor(offset int64) string {
	return b64enc([]byte("cursor:" + strconv.FormatInt(offset, 10)))
}

func gqlOffset(after string) int64 {
	if after == "" {
		return 0
	}
	raw, err := b64dec(after)
	if err != nil {
		return 0
	}
	s := string(raw)
	if !strings.HasPrefix(s, "cursor:") {
		return 0
	}
	n, _ := strconv.ParseInt(s[len("cursor:"):], 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

func (a *API) issueConn(ctx *gqlCtx, repo *Repo, args map[string]any) *gqlIssueConn {
	// states: [OPEN] / [CLOSED] / absent (all).
	var wantOpen, wantClosed bool
	if raw, ok := args["states"].([]any); ok {
		for _, s := range raw {
			switch s {
			case "OPEN":
				wantOpen = true
			case "CLOSED":
				wantClosed = true
			}
		}
	} else {
		wantOpen, wantClosed = true, true
	}
	storeState := ""
	if wantOpen && !wantClosed {
		storeState = "open"
	} else if wantClosed && !wantOpen {
		storeState = "closed"
	}
	issues := ctx.api.store.ListIssues(repo.Owner, repo.Name, storeState)
	// All timestamps are the fixed instant: creation order is the number.
	// orderBy CREATED_AT DESC (ties by number DESC) means number desc.
	desc := false
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if d, _ := ob["direction"].(string); d == "DESC" {
			desc = true
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if desc {
			return issues[i].Number > issues[j].Number
		}
		return issues[i].Number < issues[j].Number
	})
	first := argInt(args, "first")
	offset := gqlOffset(argStr(args, "after"))
	total := int64(len(issues))
	end := total
	if first > 0 && offset+first < total {
		end = offset + first
	}
	page := make([]gqlNode, 0, 8)
	for i := offset; i < end; i++ {
		page = append(page, gqlNode{kind: "Issue", val: issues[i]})
	}
	info := gqlPageInfo{hasNext: end < total}
	if len(page) > 0 {
		info.endCursor = gqlCursor(end)
	}
	return &gqlIssueConn{nodes: page, page: info, total: int(total)}
}

func (a *API) pullConn(ctx *gqlCtx, repo *Repo, args map[string]any) *gqlPullConn {
	pulls := ctx.api.store.ListPulls(repo.Owner, repo.Name, "open")
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
	first := argInt(args, "first")
	offset := gqlOffset(argStr(args, "after"))
	total := int64(len(pulls))
	end := total
	if first > 0 && offset+first < total {
		end = offset + first
	}
	page := make([]gqlNode, 0, 8)
	for i := offset; i < end; i++ {
		page = append(page, gqlNode{kind: "PullRequest", val: pulls[i]})
	}
	info := gqlPageInfo{hasNext: end < total}
	if len(page) > 0 {
		info.endCursor = gqlCursor(end)
	}
	return &gqlPullConn{nodes: page, page: info, total: int(total)}
}

// ---------------------------------------------------------------------------
// Mutations

func (a *API) resolveMutation(ctx *gqlCtx, f *gqlField, args map[string]any) *gqlNode {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		ctx.addErr("Field \"" + f.name + "\" requires an input object.")
		return &gqlNode{kind: "MutationPayload", val: nil}
	}
	switch f.name {
	case "createIssue":
		return a.mutCreateIssue(ctx, input)
	case "closeIssue":
		return a.mutCloseIssue(ctx, input)
	case "createPullRequest":
		return a.mutCreatePull(ctx, input)
	case "mergePullRequest":
		return a.mutMergePull(ctx, input)
	case "addComment":
		return a.mutAddComment(ctx, input)
	}
	return nil
}

// The mutation payload objects resolve through this kind: their fields are
// looked up in the payload map itself.
func (a *API) mutPayload(fields map[string]any) *gqlNode {
	return &gqlNode{kind: "MutationPayload", val: fields}
}

func (a *API) repoByID(id int64) *Repo {
	for _, repo := range a.store.ListRepos(a.viewer) {
		if repo.ID == id {
			return repo
		}
	}
	return nil
}

func (a *API) mutCreateIssue(ctx *gqlCtx, input map[string]any) *gqlNode {
	repoID := int64(0)
	if s, ok := input["repositoryId"].(string); ok {
		if kind, payload, ok := decodeNodeID(s); ok && kind == "Repo" {
			repoID, _ = strconv.ParseInt(payload, 10, 64)
		}
	}
	repo := a.repoByID(repoID)
	if repo == nil {
		ctx.addErr("Could not resolve to a Repository with the id provided.")
		return a.mutPayload(map[string]any{"issue": gqlNode{kind: "Issue", val: nil}})
	}
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	iss, err := a.store.CreateIssue(repo.Owner, repo.Name, title, body, nil, a.viewer)
	if err != nil {
		ctx.addErr("Could not create the issue.")
		return a.mutPayload(map[string]any{"issue": gqlNode{kind: "Issue", val: nil}})
	}
	return a.mutPayload(map[string]any{"issue": gqlNode{kind: "Issue", val: iss}})
}

func (a *API) mutCloseIssue(ctx *gqlCtx, input map[string]any) *gqlNode {
	id, _ := input["issueId"].(string)
	kind, payload, ok := decodeNodeID(id)
	if !ok || kind != "Issue" {
		ctx.addErr("Could not resolve to an Issue with the id provided.")
		return a.mutPayload(map[string]any{"issue": gqlNode{kind: "Issue", val: nil}})
	}
	owner, repo, num := splitNodeRef(payload)
	iss, ok := a.store.UpdateIssueState(owner, repo, num, "closed")
	if !ok {
		ctx.addErr("Could not resolve to an Issue with the id provided.")
		return a.mutPayload(map[string]any{"issue": gqlNode{kind: "Issue", val: nil}})
	}
	return a.mutPayload(map[string]any{"issue": gqlNode{kind: "Issue", val: iss}})
}

func (a *API) mutCreatePull(ctx *gqlCtx, input map[string]any) *gqlNode {
	repoID := int64(0)
	if s, ok := input["repositoryId"].(string); ok {
		if kind, payload, ok := decodeNodeID(s); ok && kind == "Repo" {
			repoID, _ = strconv.ParseInt(payload, 10, 64)
		}
	}
	repo := a.repoByID(repoID)
	if repo == nil {
		ctx.addErr("Could not resolve to a Repository with the id provided.")
		return a.mutPayload(map[string]any{"pullRequest": gqlNode{kind: "PullRequest", val: nil}})
	}
	title, _ := input["title"].(string)
	body, _ := input["body"].(string)
	head, _ := input["headRefName"].(string)
	base, _ := input["baseRefName"].(string)
	pull, err := a.store.CreatePull(repo.Owner, repo.Name, title, body, head, base, a.viewer)
	if err != nil {
		ctx.addErr("Could not create the pull request.")
		return a.mutPayload(map[string]any{"pullRequest": gqlNode{kind: "PullRequest", val: nil}})
	}
	return a.mutPayload(map[string]any{"pullRequest": gqlNode{kind: "PullRequest", val: pull}})
}

func (a *API) mutMergePull(ctx *gqlCtx, input map[string]any) *gqlNode {
	id, _ := input["pullRequestId"].(string)
	kind, payload, ok := decodeNodeID(id)
	if !ok || kind != "PullRequest" {
		ctx.addErr("Could not resolve to a PullRequest with the id provided.")
		return a.mutPayload(map[string]any{"pullRequest": gqlNode{kind: "PullRequest", val: nil}})
	}
	owner, repo, num := splitNodeRef(payload)
	if _, ok := a.store.GetPull(owner, repo, num); !ok {
		ctx.addErr("Could not resolve to a PullRequest with the id provided.")
		return a.mutPayload(map[string]any{"pullRequest": gqlNode{kind: "PullRequest", val: nil}})
	}
	pull, err := a.store.MergePull(owner, repo, num)
	if err != nil {
		ctx.addErr("Pull request is conflicted and cannot be merged.")
		return a.mutPayload(map[string]any{"pullRequest": gqlNode{kind: "PullRequest", val: nil}})
	}
	return a.mutPayload(map[string]any{"pullRequest": gqlNode{kind: "PullRequest", val: pull}})
}

func (a *API) mutAddComment(ctx *gqlCtx, input map[string]any) *gqlNode {
	id, _ := input["subjectId"].(string)
	kind, payload, ok := decodeNodeID(id)
	if !ok || kind != "Issue" {
		ctx.addErr("Could not resolve to an Issue with the id provided.")
		return a.mutPayload(map[string]any{"commentEdge": gqlNode{kind: "CommentEdge", val: nil}})
	}
	owner, repo, num := splitNodeRef(payload)
	body, _ := input["body"].(string)
	cid, ok := a.addComment(owner, repo, num, body, a.viewer)
	if !ok {
		ctx.addErr("Could not resolve to an Issue with the id provided.")
		return a.mutPayload(map[string]any{"commentEdge": gqlNode{kind: "CommentEdge", val: nil}})
	}
	edge := &gqlCommentEdge{id: nodeIDComment(owner, repo, num, cid), body: body}
	return a.mutPayload(map[string]any{"commentEdge": gqlNode{kind: "CommentEdge", val: edge}})
}
