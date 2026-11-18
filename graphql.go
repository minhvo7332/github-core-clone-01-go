// GraphQL v4: the documents gh and github-mcp-server actually send.
// A small recursive-descent parser for the subset those documents use
// (operation names, variables, aliases, inline fragments, inline enum lists
// and input objects, batched arrays), plus a type-driven executor over the
// same store the REST surface uses.
//
// Rules the checks pin down:
//   - viewer.login is the token owner (adaiversen, hardcoded).
//   - repository(owner:, name:) is null (with no errors) when unknown.
//   - __type fields/enumValues answer under gh's aliases with the fields
//     gh feature-detects.
//   - issues(first, states, orderBy, after): CREATED_AT DESC, ties by number
//     DESC; cursors round-trip; a page holds exactly `first` nodes.
//   - An unknown field is HTTP 200 with an errors array naming the field.
//   - Global ids are opaque (base64url payloads) and stable across reads.
package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// --- tokenizer ---

type gqlToken struct {
	kind  string // "punct" | "name" | "int" | "string"
	value string
	num   float64
}

func tokenizeGQL(src string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(src) {
		ch := src[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			tokens = append(tokens, gqlToken{kind: "punct", value: "..."})
			i += 3
		case strings.IndexByte("{}():!=$@[]", ch) >= 0:
			tokens = append(tokens, gqlToken{kind: "punct", value: string(ch)})
			i++
		case ch == '"':
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					default:
						out.WriteByte(esc)
					}
					j += 2
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, gqlToken{kind: "string", value: out.String()})
			i = j + 1
		case ch >= '0' && ch <= '9' || ch == '-':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			n, _ := strconv.ParseFloat(src[i:j], 64)
			tokens = append(tokens, gqlToken{kind: "int", value: src[i:j], num: n})
			i = j
		case ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '_':
			j := i
			for j < len(src) && (src[j] >= 'A' && src[j] <= 'Z' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= '0' && src[j] <= '9' || src[j] == '_') {
				j++
			}
			tokens = append(tokens, gqlToken{kind: "name", value: src[i:j]})
			i = j
		default:
			return nil, errGQL("unexpected character in GraphQL document")
		}
	}
	return tokens, nil
}

// --- parser ---

type gqlValue struct {
	kind   string // variable|string|int|bool|null|enum|list|object
	name   string
	str    string
	num    float64
	b      bool
	values []*gqlValue
	fields []gqlFieldVal
}

type gqlFieldVal struct {
	name  string
	value *gqlValue
}

type gqlSelection struct {
	alias        string
	hasAlias     bool
	name         string
	args         []gqlFieldVal
	selections   []*gqlSelection
	fragmentType string // "" = none; "__skip__" = named fragment spread
	hasFragment  bool
}

type gqlVarDef struct {
	name     string
	typ      string
	required bool
}

type gqlOperation struct {
	typ        string // "query" | "mutation"
	name       string
	variables  []gqlVarDef
	selections []*gqlSelection
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

type gqlError struct{ msg string }

func (e *gqlError) Error() string { return e.msg }
func errGQL(msg string) error     { return &gqlError{msg} }

func (p *gqlParser) peek() *gqlToken {
	if p.pos >= len(p.tokens) {
		return nil
	}
	return &p.tokens[p.pos]
}

func (p *gqlParser) next() (gqlToken, error) {
	if p.pos >= len(p.tokens) {
		return gqlToken{}, errGQL("unexpected end of GraphQL document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *gqlParser) expectPunct(v string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != "punct" || t.value != v {
		return errGQL("expected " + v + " in GraphQL document")
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	t, err := p.next()
	if err != nil {
		return "", err
	}
	if t.kind != "name" {
		return "", errGQL("expected a name in GraphQL document")
	}
	return t.value, nil
}

func (p *gqlParser) isPunct(v string) bool {
	t := p.peek()
	return t != nil && t.kind == "punct" && t.value == v
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, error) {
	var ops []*gqlOperation
	for p.pos < len(p.tokens) {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{typ: "query"}
	first := p.peek()
	if first != nil && first.kind == "name" && (first.value == "query" || first.value == "mutation") {
		op.typ = first.value
		p.pos++
		if n := p.peek(); n != nil && n.kind == "name" {
			op.name = n.value
			p.pos++
		}
	}
	if p.isPunct("(") {
		p.pos++
		for !p.isPunct(")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			var typeName string
			if p.isPunct("[") {
				p.pos++
				inner, err := p.expectName()
				if err != nil {
					return nil, err
				}
				typeName = "[" + inner + "]"
				if err := p.expectPunct("]"); err != nil {
					return nil, err
				}
			} else {
				typeName, err = p.expectName()
				if err != nil {
					return nil, err
				}
			}
			required := false
			if p.isPunct("!") {
				p.pos++
				required = true
			}
			if p.isPunct("=") {
				p.pos++
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
			op.variables = append(op.variables, gqlVarDef{name, typeName, required})
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = sels
	return op, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for !p.isPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if t := p.peek(); t != nil && t.kind == "punct" && t.value == "..." {
		p.pos++
		if n := p.peek(); n != nil && n.kind == "name" && n.value == "on" {
			p.pos++
			typeName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{fragmentType: typeName, hasFragment: true, selections: sels}, nil
		}
		// Named fragment spread: not used by the suite; skip.
		if _, err := p.expectName(); err != nil {
			return nil, err
		}
		return &gqlSelection{fragmentType: "__skip__", hasFragment: true}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	sel := &gqlSelection{name: first}
	if p.isPunct(":") {
		p.pos++
		sel.alias = first
		sel.hasAlias = true
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		sel.name = name
	}
	if p.isPunct("(") {
		p.pos++
		for !p.isPunct(")") {
			argName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			sel.args = append(sel.args, gqlFieldVal{argName, v})
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	if p.isPunct("{") {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = sels
	}
	return sel, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	switch {
	case t.kind == "punct" && t.value == "$":
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "variable", name: name}, nil
	case t.kind == "punct" && t.value == "[":
		v := &gqlValue{kind: "list"}
		for !p.isPunct("]") {
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.values = append(v.values, item)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return v, nil
	case t.kind == "punct" && t.value == "{":
		v := &gqlValue{kind: "object"}
		for !p.isPunct("}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.fields = append(v.fields, gqlFieldVal{name, val})
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return v, nil
	case t.kind == "string":
		return &gqlValue{kind: "string", str: t.value}, nil
	case t.kind == "int":
		return &gqlValue{kind: "int", num: t.num}, nil
	case t.kind == "name":
		switch t.value {
		case "true":
			return &gqlValue{kind: "bool", b: true}, nil
		case "false":
			return &gqlValue{kind: "bool", b: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		}
		return &gqlValue{kind: "enum", str: t.value}, nil
	}
	return nil, errGQL("unexpected value in GraphQL document")
}

// --- execution ---

type gqlVariables map[string]any

func resolveGQLValue(node *gqlValue, vars gqlVariables) any {
	switch node.kind {
	case "variable":
		if v, ok := vars[node.name]; ok {
			return v
		}
		return nil
	case "string":
		return node.str
	case "int":
		return node.num
	case "bool":
		return node.b
	case "null":
		return nil
	case "enum":
		return node.str
	case "list":
		out := make([]any, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, resolveGQLValue(v, vars))
		}
		return out
	case "object":
		out := make(map[string]any, len(node.fields))
		for _, f := range node.fields {
			out[f.name] = resolveGQLValue(f.value, vars)
		}
		return out
	}
	return nil
}

var issueFields = []string{"id", "number", "title", "state", "body"}
var pullRequestFields = []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"}
var repositoryFields = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"}
var searchTypeValues = []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "MILESTONE"}

// gqlObject is a typed value in the executor: __typename plus payload fields.
type gqlObject struct {
	typename string
	fields   map[string]any
}

func newGQLObject(typename string, fields map[string]any) *gqlObject {
	if fields == nil {
		fields = make(map[string]any)
	}
	return &gqlObject{typename: typename, fields: fields}
}

func repoGQLObject(s *Store, repo *Repo) *gqlObject {
	return newGQLObject("Repository", map[string]any{"__repo": repo, "__store": s})
}
func issueGQLObject(repo *Repo, issue *Issue) *gqlObject {
	return newGQLObject("Issue", map[string]any{"__repo": repo, "__issue": issue})
}
func pullGQLObject(repo *Repo, pr *PullRequest) *gqlObject {
	return newGQLObject("PullRequest", map[string]any{"__repo": repo, "__pr": pr})
}

type gqlFieldError struct{ msg string }

func (e *gqlFieldError) Error() string { return e.msg }

type gqlExecutor struct {
	errors []map[string]string
	store  *Store
}

func (e *gqlExecutor) execute(document string, vars gqlVariables) map[string]any {
	e.errors = nil
	tokens, err := tokenizeGQL(document)
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]string{{"message": err.Error()}}}
	}
	ops, err := newGQLParser(tokens).parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []map[string]string{{"message": err.Error()}}}
	}
	if len(ops) == 0 {
		return map[string]any{"data": nil, "errors": []map[string]string{{"message": "empty GraphQL document"}}}
	}
	op := ops[0]
	rootTyp := "Query"
	if op.typ == "mutation" {
		rootTyp = "Mutation"
	}
	root := newGQLObject(rootTyp, nil)
	data, ferr := e.executeSelections(root, op.selections, vars)
	if ferr != nil {
		if len(e.errors) == 0 {
			e.errors = append(e.errors, map[string]string{"message": "field error"})
		}
		return map[string]any{"data": nil, "errors": e.errors}
	}
	if len(e.errors) > 0 {
		return map[string]any{"data": nil, "errors": e.errors}
	}
	return map[string]any{"data": data}
}

func newGQLParser(tokens []gqlToken) *gqlParser { return &gqlParser{tokens: tokens} }

func (e *gqlExecutor) fieldError(typeName, field string) error {
	e.errors = append(e.errors, map[string]string{"message": `Cannot query field "` + field + `" on type "` + typeName + `".`})
	return &gqlFieldError{"unknown field " + field}
}

func (e *gqlExecutor) executeSelections(parent *gqlObject, sels []*gqlSelection, vars gqlVariables) (map[string]any, error) {
	out := make(map[string]any)
	for _, sel := range sels {
		if sel.hasFragment && sel.fragmentType == "__skip__" {
			continue
		}
		if sel.hasFragment {
			if sel.fragmentType != parent.typename {
				continue
			}
			sub, err := e.executeSelections(parent, sel.selections, vars)
			if err != nil {
				return nil, err
			}
			for k, v := range sub {
				out[k] = v
			}
			continue
		}
		key := sel.name
		if sel.hasAlias {
			key = sel.alias
		}
		val, err := e.resolveField(parent, sel, vars)
		if err != nil {
			if _, ok := err.(*gqlFieldError); ok {
				return nil, err
			}
			e.errors = append(e.errors, map[string]string{"message": err.Error()})
			return nil, &gqlFieldError{"field error"}
		}
		out[key] = val
	}
	return out, nil
}

func (e *gqlExecutor) argMap(sel *gqlSelection, vars gqlVariables) map[string]any {
	out := make(map[string]any, len(sel.args))
	for _, a := range sel.args {
		out[a.name] = resolveGQLValue(a.value, vars)
	}
	return out
}

func (e *gqlExecutor) sub(value any, sel *gqlSelection, vars gqlVariables) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch v := value.(type) {
	case []*gqlObject:
		out := make([]any, 0, len(v))
		for _, entry := range v {
			r, err := e.sub(entry, sel, vars)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	case *gqlObject:
		return e.executeSelections(v, sel.selections, vars)
	}
	return value, nil
}

func (e *gqlExecutor) resolveField(parent *gqlObject, sel *gqlSelection, vars gqlVariables) (any, error) {
	typeName := parent.typename
	args := e.argMap(sel, vars)
	selectVal := func(value any) (any, error) {
		if len(sel.selections) == 0 {
			return value, nil
		}
		return e.sub(value, sel, vars)
	}

	switch typeName {
	case "Query":
		switch sel.name {
		case "viewer":
			return selectVal(newGQLObject("User", map[string]any{"__login": viewerLogin}))
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			var repo *Repo
			if owner == viewerLogin {
				repo = e.store.getRepo(owner, name)
			}
			if repo == nil {
				return nil, nil
			}
			return selectVal(repoGQLObject(e.store, repo))
		case "node":
			decoded := decodeGlobalID(strVal(args["id"]))
			if decoded == nil || decoded.Kind != "R" {
				return nil, nil
			}
			repo := e.store.getRepo(decoded.Owner, decoded.Name)
			if repo == nil {
				return nil, nil
			}
			return selectVal(repoGQLObject(e.store, repo))
		case "__type":
			name, _ := args["name"].(string)
			return selectVal(newGQLObject("__Type", map[string]any{"__typeName": name}))
		}
		return nil, e.fieldError(typeName, sel.name)
	case "User":
		switch sel.name {
		case "login":
			if l, ok := parent.fields["__login"].(string); ok {
				return l, nil
			}
			return viewerLogin, nil
		case "id":
			return viewerID, nil
		}
		return nil, e.fieldError(typeName, sel.name)
	case "Repository":
		repo := parent.fields["__repo"].(*Repo)
		switch sel.name {
		case "id":
			return repoGlobalID(repo.Owner, repo.Name), nil
		case "databaseId":
			return repo.DBID, nil
		case "name":
			return repo.Name, nil
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name, nil
		case "owner":
			return selectVal(newGQLObject("User", map[string]any{"__login": repo.Owner}))
		case "defaultBranchRef":
			tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if !ok {
				_ = tip
				return nil, nil
			}
			return selectVal(newGQLObject("Ref", map[string]any{"__name": repo.DefaultBranch}))
		case "viewerPermission":
			return "WRITE", nil
		case "hasIssuesEnabled":
			return true, nil
		case "issue":
			n := numArg(args["number"])
			issue, ok := repo.Issues[n]
			if !ok {
				return nil, nil
			}
			return selectVal(issueGQLObject(repo, issue))
		case "issues":
			return selectVal(e.issueConnection(repo, args))
		case "pullRequests":
			return selectVal(e.pullConnection(repo, args))
		case "pullRequest":
			n := numArg(args["number"])
			pr, ok := repo.Pulls[n]
			if !ok {
				return nil, nil
			}
			return selectVal(pullGQLObject(repo, pr))
		}
		return nil, e.fieldError(typeName, sel.name)
	case "Ref":
		if sel.name == "name" {
			return parent.fields["__name"], nil
		}
		return nil, e.fieldError(typeName, sel.name)
	case "Issue":
		issue := parent.fields["__issue"].(*Issue)
		switch sel.name {
		case "id":
			return issue.GlobalID, nil
		case "number":
			return issue.Number, nil
		case "title":
			return issue.Title, nil
		case "state":
			if issue.State == "open" {
				return "OPEN", nil
			}
			return "CLOSED", nil
		case "body":
			return issue.Body, nil
		}
		return nil, e.fieldError(typeName, sel.name)
	case "IssueConnection":
		switch sel.name {
		case "nodes":
			return selectVal(parent.fields["__nodes"])
		case "pageInfo":
			return selectVal(parent.fields["__pageInfo"])
		}
		return nil, e.fieldError(typeName, sel.name)
	case "PageInfo":
		switch sel.name {
		case "hasNextPage":
			return parent.fields["__hasNextPage"], nil
		case "endCursor":
			return parent.fields["__endCursor"], nil
		}
		return nil, e.fieldError(typeName, sel.name)
	case "PullRequest":
		pr := parent.fields["__pr"].(*PullRequest)
		repo := parent.fields["__repo"].(*Repo)
		switch sel.name {
		case "id":
			return pr.GlobalID, nil
		case "number":
			return pr.Number, nil
		case "state":
			if pr.State == "open" {
				return "OPEN", nil
			}
			return "CLOSED", nil
		case "merged":
			return pr.Merged, nil
		case "headRefName":
			return pr.Head, nil
		case "baseRefName":
			return pr.Base, nil
		case "mergeable":
			return e.mergeableOf(repo, pr), nil
		case "files":
			first := 10
			if n, ok := args["first"].(float64); ok {
				first = int(n)
			}
			files := prFilesOf(e.store, repo, pr)
			if len(files) > first {
				files = files[:first]
			}
			nodes := make([]*gqlObject, 0, len(files))
			for _, f := range files {
				nodes = append(nodes, newGQLObject("PullRequestChangedFile", map[string]any{"__path": f}))
			}
			return selectVal(newGQLObject("PullRequestChangedFileConnection", map[string]any{"__nodes": nodes}))
		}
		return nil, e.fieldError(typeName, sel.name)
	case "PullRequestConnection":
		if sel.name == "nodes" {
			return selectVal(parent.fields["__nodes"])
		}
		return nil, e.fieldError(typeName, sel.name)
	case "PullRequestChangedFileConnection":
		if sel.name == "nodes" {
			return selectVal(parent.fields["__nodes"])
		}
		return nil, e.fieldError(typeName, sel.name)
	case "PullRequestChangedFile":
		if sel.name == "path" {
			return parent.fields["__path"], nil
		}
		return nil, e.fieldError(typeName, sel.name)
	case "__Type":
		name, _ := parent.fields["__typeName"].(string)
		switch sel.name {
		case "fields":
			var list []string
			switch name {
			case "Issue":
				list = issueFields
			case "PullRequest":
				list = pullRequestFields
			case "Repository":
				list = repositoryFields
			}
			nodes := make([]*gqlObject, 0, len(list))
			for _, entry := range list {
				nodes = append(nodes, newGQLObject("__Field", map[string]any{"__name": entry}))
			}
			return selectVal(nodes)
		case "enumValues":
			var list []string
			if name == "SearchType" {
				list = searchTypeValues
			}
			nodes := make([]*gqlObject, 0, len(list))
			for _, entry := range list {
				nodes = append(nodes, newGQLObject("__EnumValue", map[string]any{"__name": entry}))
			}
			return selectVal(nodes)
		case "name":
			return name, nil
		}
		return nil, e.fieldError(typeName, sel.name)
	case "__Field", "__EnumValue":
		if sel.name == "name" {
			return parent.fields["__name"], nil
		}
		return nil, e.fieldError(typeName, sel.name)
	case "Mutation":
		switch sel.name {
		case "createIssue":
			v, err := e.createIssue(args)
			if err != nil {
				return nil, err
			}
			return selectVal(v)
		case "closeIssue":
			v, err := e.closeIssue(args)
			if err != nil {
				return nil, err
			}
			return selectVal(v)
		case "createPullRequest":
			v, err := e.createPullRequest(args)
			if err != nil {
				return nil, err
			}
			return selectVal(v)
		case "mergePullRequest":
			v, err := e.mergePullRequest(args)
			if err != nil {
				return nil, err
			}
			return selectVal(v)
		case "addComment":
			v, err := e.addComment(args)
			if err != nil {
				return nil, err
			}
			return selectVal(v)
		}
		return nil, e.fieldError(typeName, sel.name)
	case "CreateIssuePayload", "CloseIssuePayload":
		if sel.name == "issue" {
			return selectVal(parent.fields["__issue"])
		}
		return nil, e.fieldError(typeName, sel.name)
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if sel.name == "pullRequest" {
			return selectVal(parent.fields["__pr"])
		}
		return nil, e.fieldError(typeName, sel.name)
	case "AddCommentPayload":
		if sel.name == "commentEdge" {
			return selectVal(parent.fields["__edge"])
		}
		return nil, e.fieldError(typeName, sel.name)
	case "IssueCommentEdge":
		if sel.name == "node" {
			return selectVal(parent.fields["__node"])
		}
		return nil, e.fieldError(typeName, sel.name)
	case "IssueComment":
		switch sel.name {
		case "id":
			return parent.fields["__id"], nil
		case "body":
			return parent.fields["__body"], nil
		}
		return nil, e.fieldError(typeName, sel.name)
	}
	return nil, e.fieldError(typeName, sel.name)
}

func numArg(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func (e *gqlExecutor) issueConnection(repo *Repo, args map[string]any) *gqlObject {
	list := make([]*Issue, 0, len(repo.Issues))
	for _, iss := range repo.Issues {
		list = append(list, iss)
	}
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := make(map[string]bool)
		for _, s := range states {
			if str, ok := s.(string); ok {
				wanted[strings.ToLower(str)] = true
			}
		}
		filtered := list[:0]
		for _, iss := range list {
			if wanted[iss.State] {
				filtered = append(filtered, iss)
			}
		}
		list = filtered
	}
	direction := "DESC"
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if d, ok := ob["direction"].(string); ok {
			direction = d
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if direction == "ASC" {
			if list[i].Seq != list[j].Seq {
				return list[i].Seq < list[j].Seq
			}
			return list[i].Number < list[j].Number
		}
		if list[i].Seq != list[j].Seq {
			return list[i].Seq > list[j].Seq
		}
		return list[i].Number > list[j].Number
	})
	first := len(list)
	if n, ok := args["first"].(float64); ok {
		first = int(n)
	}
	start := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if seq, ok := decodeCursor(after); ok {
			for i, iss := range list {
				if iss.Seq == seq {
					start = i + 1
					break
				}
			}
		}
	}
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	if start > len(list) {
		start = len(list)
		end = start
	}
	page := list[start:end]
	hasNext := start+len(page) < len(list)
	var endCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].Seq)
	}
	nodes := make([]*gqlObject, 0, len(page))
	for _, iss := range page {
		nodes = append(nodes, issueGQLObject(repo, iss))
	}
	return newGQLObject("IssueConnection", map[string]any{
		"__nodes": nodes,
		"__pageInfo": newGQLObject("PageInfo", map[string]any{
			"__hasNextPage": hasNext, "__endCursor": endCursor,
		}),
	})
}

func (e *gqlExecutor) pullConnection(repo *Repo, args map[string]any) *gqlObject {
	list := make([]*PullRequest, 0, len(repo.Pulls))
	for _, pr := range repo.Pulls {
		if pr.State == "open" {
			list = append(list, pr)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Seq != list[j].Seq {
			return list[i].Seq > list[j].Seq
		}
		return list[i].Number > list[j].Number
	})
	first := len(list)
	if n, ok := args["first"].(float64); ok {
		first = int(n)
	}
	if first > len(list) {
		first = len(list)
	}
	if first < 0 {
		first = 0
	}
	nodes := make([]*gqlObject, 0, first)
	for _, pr := range list[:first] {
		nodes = append(nodes, pullGQLObject(repo, pr))
	}
	return newGQLObject("PullRequestConnection", map[string]any{"__nodes": nodes})
}

func (e *gqlExecutor) mergeableOf(repo *Repo, pr *PullRequest) string {
	if pr.State == "closed" {
		return "UNKNOWN"
	}
	if wouldConflict(e.store, repo, pr) {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

func inputMap(args map[string]any) map[string]any {
	if m, ok := args["input"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func (e *gqlExecutor) createIssue(args map[string]any) (*gqlObject, error) {
	input := inputMap(args)
	decoded := decodeGlobalID(strVal(input["repositoryId"]))
	if decoded == nil || decoded.Kind != "R" {
		return nil, errGQL("createIssue: unknown repository")
	}
	repo := e.store.getRepo(decoded.Owner, decoded.Name)
	if repo == nil {
		return nil, errGQL("createIssue: unknown repository")
	}
	number := repo.NextNumber
	repo.NextNumber++
	now := nowISO()
	issue := &Issue{
		Number: number, GlobalID: issueGlobalID(repo.Owner, repo.Name, number),
		Title: strVal(input["title"]), Body: strVal(input["body"]), State: "open",
		Labels: []*Label{}, Comments: []*Comment{},
		Seq: e.store.issueSeq, CreatedAt: now, UpdatedAt: now,
	}
	e.store.issueSeq++
	repo.Issues[number] = issue
	return newGQLObject("CreateIssuePayload", map[string]any{"__issue": issueGQLObject(repo, issue)}), nil
}

func (e *gqlExecutor) closeIssue(args map[string]any) (*gqlObject, error) {
	input := inputMap(args)
	decoded := decodeGlobalID(strVal(input["issueId"]))
	if decoded == nil || decoded.Kind != "I" {
		return nil, errGQL("closeIssue: unknown issue")
	}
	repo := e.store.getRepo(decoded.Owner, decoded.Name)
	issue := (*Issue)(nil)
	if repo != nil {
		issue = repo.Issues[decoded.Number]
	}
	if issue == nil {
		return nil, errGQL("closeIssue: unknown issue")
	}
	issue.State = "closed"
	issue.UpdatedAt = nowISO()
	return newGQLObject("CloseIssuePayload", map[string]any{"__issue": issueGQLObject(repo, issue)}), nil
}

func (e *gqlExecutor) createPullRequest(args map[string]any) (*gqlObject, error) {
	input := inputMap(args)
	decoded := decodeGlobalID(strVal(input["repositoryId"]))
	if decoded == nil || decoded.Kind != "R" {
		return nil, errGQL("createPullRequest: unknown repository")
	}
	repo := e.store.getRepo(decoded.Owner, decoded.Name)
	if repo == nil {
		return nil, errGQL("createPullRequest: unknown repository")
	}
	head := strVal(input["headRefName"])
	base := strVal(input["baseRefName"])
	headSHA, ok1 := repo.Refs["refs/heads/"+head]
	baseSHA, ok2 := repo.Refs["refs/heads/"+base]
	if !ok1 || !ok2 {
		return nil, errGQL("createPullRequest: unknown branch")
	}
	for _, existing := range repo.Pulls {
		if existing.Head == head && existing.Base == base && existing.State == "open" {
			return nil, errGQL("createPullRequest: a pull request already exists for this head and base")
		}
	}
	number := repo.NextNumber
	repo.NextNumber++
	now := nowISO()
	pr := &PullRequest{
		Number: number, GlobalID: pullGlobalID(repo.Owner, repo.Name, number),
		Title: strVal(input["title"]), Body: strVal(input["body"]), State: "open",
		Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA,
		Seq: e.store.issueSeq, CreatedAt: now, UpdatedAt: now,
	}
	e.store.issueSeq++
	repo.Pulls[number] = pr
	return newGQLObject("CreatePullRequestPayload", map[string]any{"__pr": pullGQLObject(repo, pr)}), nil
}

func (e *gqlExecutor) mergePullRequest(args map[string]any) (*gqlObject, error) {
	input := inputMap(args)
	decoded := decodeGlobalID(strVal(input["pullRequestId"]))
	if decoded == nil || decoded.Kind != "PR" {
		return nil, errGQL("mergePullRequest: unknown pull request")
	}
	repo := e.store.getRepo(decoded.Owner, decoded.Name)
	var pr *PullRequest
	if repo != nil {
		pr = repo.Pulls[decoded.Number]
	}
	if pr == nil || repo == nil {
		return nil, errGQL("mergePullRequest: unknown pull request")
	}
	if pr.State == "closed" {
		return nil, errGQL("mergePullRequest: pull request is already closed")
	}
	conflict, sha := squashMerge(e.store, repo, pr)
	if conflict {
		return nil, errGQL("mergePullRequest: merge conflict")
	}
	if sha == "" {
		return nil, errGQL("mergePullRequest: cannot merge")
	}
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = nowISO()
	pr.MergeCommitSHA = sha
	pr.BaseSHA = sha
	pr.UpdatedAt = nowISO()
	return newGQLObject("MergePullRequestPayload", map[string]any{"__pr": pullGQLObject(repo, pr)}), nil
}

func (e *gqlExecutor) addComment(args map[string]any) (*gqlObject, error) {
	input := inputMap(args)
	decoded := decodeGlobalID(strVal(input["subjectId"]))
	if decoded == nil || decoded.Kind != "I" {
		return nil, errGQL("addComment: unknown subject")
	}
	repo := e.store.getRepo(decoded.Owner, decoded.Name)
	var issue *Issue
	if repo != nil {
		issue = repo.Issues[decoded.Number]
	}
	if issue == nil || repo == nil {
		return nil, errGQL("addComment: unknown issue")
	}
	id := repo.NextCommentID
	repo.NextCommentID++
	comment := &Comment{
		ID: id, GlobalID: commentGlobalID(repo.Owner, repo.Name, issue.Number, id),
		Body: strVal(input["body"]), CreatedAt: nowISO(),
	}
	issue.Comments = append(issue.Comments, comment)
	edge := newGQLObject("IssueCommentEdge", map[string]any{
		"__node": newGQLObject("IssueComment", map[string]any{"__id": comment.GlobalID, "__body": comment.Body}),
	})
	return newGQLObject("AddCommentPayload", map[string]any{"__edge": edge}), nil
}

func encodeCursor(seq int) string {
	return b64urlEncode("cursor:" + itoa(seq))
}

func decodeCursor(cursor string) (int, bool) {
	text, ok := b64urlDecode(cursor)
	if !ok || !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	return atoiStrict(text[len("cursor:"):])
}

// wouldConflict is the three-way conflict check without mutating anything.
func wouldConflict(s *Store, repo *Repo, pr *PullRequest) bool {
	baseTip, ok1 := repo.Refs["refs/heads/"+pr.Base]
	headTip, ok2 := repo.Refs["refs/heads/"+pr.Head]
	if !ok1 || !ok2 {
		return false
	}
	baseObj := s.getObject(baseTip)
	headObj := s.getObject(headTip)
	if baseObj == nil || headObj == nil {
		return false
	}
	mb := s.mergeBase(baseTip, headTip)
	var baseTree map[string]TreeEntry
	if mb == "" {
		baseTree = make(map[string]TreeEntry)
	} else {
		baseTree = s.flattenTree(parseCommit(s.getObject(mb).data).Tree, "", nil)
	}
	headTree := s.flattenTree(parseCommit(headObj.data).Tree, "", nil)
	targetTree := s.flattenTree(parseCommit(baseObj.data).Tree, "", nil)
	paths := make(map[string]bool)
	for k := range baseTree {
		paths[k] = true
	}
	for k := range headTree {
		paths[k] = true
	}
	for path := range paths {
		baseOID := ""
		if e, ok := baseTree[path]; ok {
			baseOID = e.OID
		}
		headOID := ""
		if e, ok := headTree[path]; ok {
			headOID = e.OID
		}
		targetOID := ""
		if e, ok := targetTree[path]; ok {
			targetOID = e.OID
		}
		if headOID == baseOID {
			continue
		}
		if targetOID == baseOID {
			continue
		}
		if headOID == targetOID {
			continue
		}
		return true
	}
	return false
}

func prFilesOf(s *Store, repo *Repo, pr *PullRequest) []string {
	baseTip, ok1 := repo.Refs["refs/heads/"+pr.Base]
	headTip, ok2 := repo.Refs["refs/heads/"+pr.Head]
	if !ok1 || !ok2 {
		return nil
	}
	mb := s.mergeBase(baseTip, headTip)
	var baseTree map[string]TreeEntry
	if mb == "" {
		baseTree = make(map[string]TreeEntry)
	} else {
		baseTree = s.flattenTree(parseCommit(s.getObject(mb).data).Tree, "", nil)
	}
	headTree := s.flattenTree(parseCommit(s.getObject(headTip).data).Tree, "", nil)
	paths := make(map[string]bool)
	for k := range baseTree {
		paths[k] = true
	}
	for k := range headTree {
		paths[k] = true
	}
	sorted := make([]string, 0, len(paths))
	for k := range paths {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []string
	for _, path := range sorted {
		b, hasB := baseTree[path]
		h, hasH := headTree[path]
		if hasB && hasH && b.OID == h.OID {
			continue
		}
		out = append(out, path)
	}
	return out
}

// handleGraphQL executes one GraphQL request body (single operation or a
// batch array). Returns the HTTP status and the payload.
func handleGraphQL(s *Store, body any) (int, any) {
	executor := &gqlExecutor{store: s}
	if arr, ok := body.([]any); ok {
		results := make([]any, 0, len(arr))
		for _, entry := range arr {
			m, ok := entry.(map[string]any)
			if !ok {
				results = append(results, map[string]any{"data": nil, "errors": []map[string]string{{"message": "invalid operation"}}})
				continue
			}
			query, ok := m["query"].(string)
			if !ok {
				results = append(results, map[string]any{"data": nil, "errors": []map[string]string{{"message": "missing query"}}})
				continue
			}
			vars := gqlVariables{}
			if v, ok := m["variables"].(map[string]any); ok {
				vars = v
			}
			results = append(results, executor.execute(query, vars))
		}
		return 200, results
	}
	m, ok := body.(map[string]any)
	if !ok {
		return 400, map[string]any{"errors": []map[string]string{{"message": "invalid GraphQL body"}}}
	}
	query, ok := m["query"].(string)
	if !ok {
		return 400, map[string]any{"errors": []map[string]string{{"message": "missing query"}}}
	}
	vars := gqlVariables{}
	if v, ok := m["variables"].(map[string]any); ok {
		vars = v
	}
	return 200, executor.execute(query, vars)
}
