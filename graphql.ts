// GraphQL v4 for the documents gh and github-mcp-server actually send: a small
// hand-rolled parser (operations, aliases, variables, inline input objects and
// lists, inline fragments) over a resolver that reads the same store as REST.
// Unknown fields are GraphQL errors (HTTP 200 + errors array), never HTTP 400.

import {
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';
import { commitDistances, flattenTree, parseCommit, touchIssue } from './store.ts';

// -------------------------------------------------------------------------------------------
// Parser
// -------------------------------------------------------------------------------------------

type Value = string | number | boolean | null | { kind: 'var'; name: string } | { kind: 'enum'; value: string } | Value[] | { [key: string]: Value };

interface Selection {
  alias: string | null;
  name: string;
  args: Record<string, Value>;
  selections: Selection[];
  fragmentType: string | null;
}

interface Operation {
  type: 'query' | 'mutation';
  name: string | null;
  selections: Selection[];
}

function tokenize(input: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < input.length) {
    const ch = input[i]!;
    if (ch === ' ' || ch === '\n' || ch === '\r' || ch === '\t' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < input.length && input[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && input.slice(i, i + 3) === '...') {
      tokens.push('...');
      i += 3;
      continue;
    }
    if ('{}():[]!=$@'.includes(ch)) {
      tokens.push(ch);
      i += 1;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < input.length && input[j] !== '"') {
        if (input[j] === '\\' && j + 1 < input.length) {
          const esc = input[j + 1]!;
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          out += input[j]!;
          j += 1;
        }
      }
      tokens.push(JSON.stringify(out));
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch) && (ch !== '-' || /[0-9]/.test(input[i + 1] ?? ''))) {
      let j = i;
      while (j < input.length && /[0-9.eE+-]/.test(input[j]!)) j += 1;
      tokens.push(input.slice(i, j));
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < input.length && /[A-Za-z0-9_]/.test(input[j]!)) j += 1;
      tokens.push(input.slice(i, j));
      i = j;
      continue;
    }
    i += 1;
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: string[];

  constructor(tokens: string[]) {
    this.tokens = tokens;
  }

  peek(): string | undefined {
    return this.tokens[this.pos];
  }

  next(): string {
    const token = this.tokens[this.pos] ?? '';
    this.pos += 1;
    return token;
  }

  expect(token: string): void {
    if (this.next() !== token) throw new Error(`expected ${token}`);
  }

  parseDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.pos < this.tokens.length) {
      operations.push(this.parseOperation());
    }
    return operations;
  }

  parseOperation(): Operation {
    let type: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const token = this.peek();
    if (token === 'query' || token === 'mutation') {
      type = this.next() as 'query' | 'mutation';
      const maybeName = this.peek();
      if (maybeName !== undefined && maybeName !== '(' && maybeName !== '{') name = this.next();
    } else if (token === '{') {
      type = 'query';
    }
    if (this.peek() === '(') {
      this.next();
      let depth = 1;
      while (depth > 0 && this.pos < this.tokens.length) {
        const tok = this.next();
        if (tok === '(') depth += 1;
        else if (tok === ')') depth -= 1;
      }
    }
    const selections = this.parseSelectionSet();
    return { type, name, selections };
  }

  parseSelectionSet(): Selection[] {
    this.expect('{');
    const selections: Selection[] = [];
    while (this.peek() !== '}' && this.pos < this.tokens.length) {
      selections.push(this.parseSelection());
    }
    this.expect('}');
    return selections;
  }

  parseSelection(): Selection {
    if (this.peek() === '...') {
      this.next();
      let fragmentType: string | null = null;
      if (this.peek() === 'on') {
        this.next();
        fragmentType = this.next();
      }
      const selections = this.parseSelectionSet();
      return { alias: null, name: '__fragment', args: {}, selections, fragmentType };
    }
    let name = this.next();
    let alias: string | null = null;
    if (this.peek() === ':') {
      this.next();
      alias = name;
      name = this.next();
    }
    const args: Record<string, Value> = {};
    if (this.peek() === '(') {
      this.next();
      while (this.peek() !== ')' && this.pos < this.tokens.length) {
        const argName = this.next();
        this.expect(':');
        args[argName] = this.parseValue();
      }
      this.expect(')');
    }
    let selections: Selection[] = [];
    if (this.peek() === '{') selections = this.parseSelectionSet();
    return { alias, name, args, selections, fragmentType: null };
  }

  parseValue(): Value {
    const token = this.next();
    if (token === '$') return { kind: 'var', name: this.next() };
    if (token === '[') {
      const items: Value[] = [];
      while (this.peek() !== ']' && this.pos < this.tokens.length) items.push(this.parseValue());
      this.expect(']');
      return items;
    }
    if (token === '{') {
      const obj: Record<string, Value> = {};
      while (this.peek() !== '}' && this.pos < this.tokens.length) {
        const key = this.next();
        this.expect(':');
        obj[key] = this.parseValue();
      }
      this.expect('}');
      return obj;
    }
    if (token === 'true') return true;
    if (token === 'false') return false;
    if (token === 'null') return null;
    if (token.startsWith('"')) return JSON.parse(token) as string;
    if (/^-?[0-9]/.test(token)) return Number(token);
    return { kind: 'enum', value: token };
  }
}

function resolveValue(value: Value, variables: Record<string, unknown>): unknown {
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    if ((value as { kind?: string }).kind === 'var') return variables[(value as { kind: string; name: string }).name];
    if ((value as { kind?: string }).kind === 'enum') return (value as { kind: string; value: string }).value;
    const out: Record<string, unknown> = {};
    for (const [key, child] of Object.entries(value)) out[key] = resolveValue(child as Value, variables);
    return out;
  }
  if (Array.isArray(value)) return value.map((child) => resolveValue(child, variables));
  return value;
}

function resolveArgs(selection: Selection, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(selection.args)) out[key] = resolveValue(value, variables);
  return out;
}

// -------------------------------------------------------------------------------------------
// Introspection tables
// -------------------------------------------------------------------------------------------

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'url', 'author', 'labels', 'comments'],
  PullRequest: ['id', 'number', 'title', 'state', 'body', 'headRefName', 'baseRefName', 'mergeable', 'merged', 'files', 'headRef', 'baseRef'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'url'],
  SearchType: [],
  User: ['login', 'id', 'name', 'email'],
};

const TYPE_ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'PULL_REQUEST', 'USER', 'REPOSITORY', 'COMMIT', 'DISCUSSION'],
  IssueState: ['OPEN', 'CLOSED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
};

// -------------------------------------------------------------------------------------------
// Executor
// -------------------------------------------------------------------------------------------

interface Ctx {
  store: Store;
  variables: Record<string, unknown>;
  errors: Array<{ message: string }>;
}

function fieldError(ctx: Ctx, typeName: string, field: string): void {
  ctx.errors.push({ message: `Cannot query field "${field}" on type "${typeName}".` });
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function mergeableOf(repo: Repo, pull: Pull): string {
  if (pull.merged) return 'MERGEABLE';
  const headObj = repo.objects.get(pull.headSha);
  const baseObj = repo.objects.get(pull.baseSha);
  if (headObj === undefined || baseObj === undefined) return 'UNKNOWN';
  const baseTree = parseCommit(baseObj.data).tree;
  const headTree = parseCommit(headObj.data).tree;
  const baseFiles = new Map<string, Buffer>();
  flattenTree(repo.objects, baseTree, '', baseFiles);
  const headFiles = new Map<string, Buffer>();
  flattenTree(repo.objects, headTree, '', headFiles);
  // Conflict: the merge base and the head both changed the same path differently.
  if (pull.mergeBase !== null) {
    const baseCommit = repo.objects.get(pull.mergeBase);
    if (baseCommit !== undefined) {
      const mergeFiles = new Map<string, Buffer>();
      flattenTree(repo.objects, parseCommit(baseCommit.data).tree, '', mergeFiles);
      for (const [path, headData] of headFiles) {
        const mergeData = mergeFiles.get(path);
        const currentData = baseFiles.get(path);
        if (mergeData !== undefined && currentData !== undefined && !mergeData.equals(headData) && !mergeData.equals(currentData) && !headData.equals(currentData)) {
          return 'CONFLICTING';
        }
      }
    }
  }
  return 'MERGEABLE';
}

function pullFiles(repo: Repo, pull: Pull, first?: number): Array<{ path: string }> {
  const headObj = repo.objects.get(pull.headSha);
  if (headObj === undefined) return [];
  const headTree = parseCommit(headObj.data).tree;
  let baseTree: string | undefined;
  if (pull.mergeBase !== null) {
    const baseCommit = repo.objects.get(pull.mergeBase);
    if (baseCommit !== undefined) baseTree = parseCommit(baseCommit.data).tree;
  }
  const diff = repo.diffTrees(baseTree, headTree);
  const files = diff.filter((entry) => entry.status !== 'removed').map((entry) => ({ path: entry.path }));
  return first === undefined ? files : files.slice(0, first);
}

function resolveIssue(ctx: Ctx, repo: Repo, issue: Issue, selections: Selection[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.name === '__fragment') {
      Object.assign(out, resolveIssue(ctx, repo, issue, sel.selections));
      continue;
    }
    const key = sel.alias ?? sel.name;
    switch (sel.name) {
      case 'id':
        out[key] = ctx.store.globalId('issue', `${repo.owner}/${repo.name}:${issue.number}`);
        break;
      case 'number':
        out[key] = issue.number;
        break;
      case 'title':
        out[key] = issue.title;
        break;
      case 'state':
        out[key] = issueState(issue);
        break;
      case 'body':
        out[key] = issue.body;
        break;
      case 'url':
        out[key] = `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`;
        break;
      default:
        fieldError(ctx, 'Issue', sel.name);
        break;
    }
  }
  return out;
}

function resolvePull(ctx: Ctx, repo: Repo, pull: Pull, selections: Selection[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.name === '__fragment') {
      Object.assign(out, resolvePull(ctx, repo, pull, sel.selections));
      continue;
    }
    const key = sel.alias ?? sel.name;
    const args = resolveArgs(sel, ctx.variables);
    switch (sel.name) {
      case 'id':
        out[key] = ctx.store.globalId('pull', `${repo.owner}/${repo.name}:${pull.number}`);
        break;
      case 'number':
        out[key] = pull.number;
        break;
      case 'title':
        out[key] = pull.title;
        break;
      case 'state':
        out[key] = pullState(pull);
        break;
      case 'body':
        out[key] = pull.body;
        break;
      case 'headRefName':
        out[key] = pull.head;
        break;
      case 'baseRefName':
        out[key] = pull.base;
        break;
      case 'mergeable':
        out[key] = mergeableOf(repo, pull);
        break;
      case 'merged':
        out[key] = pull.merged;
        break;
      case 'files': {
        const first = typeof args.first === 'number' ? args.first : undefined;
        const nodes = pullFiles(repo, pull, first);
        out[key] = { nodes, totalCount: nodes.length };
        break;
      }
      default:
        fieldError(ctx, 'PullRequest', sel.name);
        break;
    }
  }
  return out;
}

function resolveIssuesConnection(ctx: Ctx, repo: Repo, selections: Selection[], args: Record<string, unknown>): Record<string, unknown> {
  let issues = [...repo.issues];
  const states = args.states;
  if (Array.isArray(states)) {
    const wanted = new Set(states.map((state) => String(state).toLowerCase()));
    issues = issues.filter((issue) => wanted.has(issue.state));
  }
  const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
  if (orderBy !== undefined && String(orderBy.direction).toUpperCase() === 'DESC') {
    issues.sort((a, b) => b.number - a.number);
  } else if (orderBy !== undefined) {
    issues.sort((a, b) => a.number - b.number);
  } else {
    issues.sort((a, b) => a.number - b.number);
  }
  const first = typeof args.first === 'number' ? args.first : undefined;
  const after = typeof args.after === 'string' ? args.after : null;
  let start = 0;
  if (after !== null && after !== '') {
    try {
      const decoded = Buffer.from(after, 'base64url').toString('utf8');
      const n = Number.parseInt(decoded.replace('cursor:', ''), 10);
      if (Number.isFinite(n)) {
        const index = issues.findIndex((issue) => issue.number === n);
        if (index >= 0) start = index + 1;
      }
    } catch {
      // An opaque cursor we cannot read is treated as the start.
    }
  }
  const page = first === undefined ? issues.slice(start) : issues.slice(start, start + first);
  const hasNextPage = start + page.length < issues.length;
  const endCursor = page.length > 0 ? Buffer.from(`cursor:${page[page.length - 1]!.number}`, 'utf8').toString('base64url') : null;
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.name === '__fragment') continue;
    const key = sel.alias ?? sel.name;
    if (sel.name === 'nodes') out[key] = page.map((issue) => resolveIssue(ctx, repo, issue, sel.selections));
    else if (sel.name === 'pageInfo') {
      const info: Record<string, unknown> = {};
      for (const sub of sel.selections) {
        const subKey = sub.alias ?? sub.name;
        if (sub.name === 'hasNextPage') info[subKey] = hasNextPage;
        else if (sub.name === 'endCursor') info[subKey] = endCursor;
        else if (sub.name === 'hasPreviousPage') info[subKey] = start > 0;
        else if (sub.name === 'startCursor') info[subKey] = page.length > 0 ? Buffer.from(`cursor:${page[0]!.number}`, 'utf8').toString('base64url') : null;
      }
      out[key] = info;
    } else if (sel.name === 'totalCount') out[key] = issues.length;
    else fieldError(ctx, 'IssueConnection', sel.name);
  }
  return out;
}

function resolvePullsConnection(ctx: Ctx, repo: Repo, selections: Selection[], args: Record<string, unknown>): Record<string, unknown> {
  let pulls = repo.pulls.filter((pull) => pull.state === 'open');
  const first = typeof args.first === 'number' ? args.first : undefined;
  if (first !== undefined) pulls = pulls.slice(0, first);
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.name === '__fragment') continue;
    const key = sel.alias ?? sel.name;
    if (sel.name === 'nodes') out[key] = pulls.map((pull) => resolvePull(ctx, repo, pull, sel.selections));
    else if (sel.name === 'totalCount') out[key] = pulls.length;
    else fieldError(ctx, 'PullRequestConnection', sel.name);
  }
  return out;
}

function resolveRepository(ctx: Ctx, repo: Repo, selections: Selection[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.name === '__fragment') {
      if (sel.fragmentType === null || sel.fragmentType === 'Repository') Object.assign(out, resolveRepository(ctx, repo, sel.selections));
      continue;
    }
    const key = sel.alias ?? sel.name;
    const args = resolveArgs(sel, ctx.variables);
    switch (sel.name) {
      case 'id':
        out[key] = ctx.store.globalId('repo', `${repo.owner}/${repo.name}`);
        break;
      case 'databaseId':
        out[key] = repo.id;
        break;
      case 'name':
        out[key] = repo.name;
        break;
      case 'nameWithOwner':
        out[key] = `${repo.owner}/${repo.name}`;
        break;
      case 'description':
        out[key] = repo.description;
        break;
      case 'url':
        out[key] = `https://github.com/${repo.owner}/${repo.name}`;
        break;
      case 'owner': {
        const owner: Record<string, unknown> = {};
        for (const sub of sel.selections) {
          const subKey = sub.alias ?? sub.name;
          if (sub.name === 'login') owner[subKey] = repo.owner;
          else if (sub.name === 'id') owner[subKey] = VIEWER_ID;
          else fieldError(ctx, 'User', sub.name);
        }
        out[key] = owner;
        break;
      }
      case 'defaultBranchRef': {
        const tip = repo.branchTip(repo.defaultBranch);
        if (tip === undefined) {
          out[key] = null;
          break;
        }
        const ref: Record<string, unknown> = {};
        for (const sub of sel.selections) {
          const subKey = sub.alias ?? sub.name;
          if (sub.name === 'name') ref[subKey] = repo.defaultBranch;
          else if (sub.name === 'target') ref[subKey] = { oid: tip };
          else fieldError(ctx, 'Ref', sub.name);
        }
        out[key] = ref;
        break;
      }
      case 'viewerPermission':
        out[key] = 'ADMIN';
        break;
      case 'hasIssuesEnabled':
        out[key] = true;
        break;
      case 'issue': {
        const number = typeof args.number === 'number' ? args.number : -1;
        const issue = repo.issues.find((entry) => entry.number === number);
        out[key] = issue === undefined ? null : resolveIssue(ctx, repo, issue, sel.selections);
        break;
      }
      case 'issues':
        out[key] = resolveIssuesConnection(ctx, repo, sel.selections, args);
        break;
      case 'pullRequests':
        out[key] = resolvePullsConnection(ctx, repo, sel.selections, args);
        break;
      case 'pullRequest': {
        const number = typeof args.number === 'number' ? args.number : -1;
        const pull = repo.pulls.find((entry) => entry.number === number);
        out[key] = pull === undefined ? null : resolvePull(ctx, repo, pull, sel.selections);
        break;
      }
      default:
        fieldError(ctx, 'Repository', sel.name);
        break;
    }
  }
  return out;
}

function resolveType(ctx: Ctx, name: string, selections: Selection[]): Record<string, unknown> | null {
  const fields = TYPE_FIELDS[name];
  const enumValues = TYPE_ENUM_VALUES[name];
  if (fields === undefined && enumValues === undefined) return null;
  const out: Record<string, unknown> = { name };
  for (const sel of selections) {
    const key = sel.alias ?? sel.name;
    if (sel.name === 'fields') {
      const includeDeprecated = true;
      void includeDeprecated;
      out[key] = (fields ?? []).map((field) => ({ name: field }));
    } else if (sel.name === 'enumValues') {
      out[key] = (enumValues ?? []).map((value) => ({ name: value }));
    } else if (sel.name === 'name') {
      out[key] = name;
    } else if (sel.name === 'kind') {
      out[key] = enumValues !== undefined ? 'ENUM' : 'OBJECT';
    }
  }
  return out;
}

// -------------------------------------------------------------------------------------------
// Mutations (they write through the same store REST uses)
// -------------------------------------------------------------------------------------------

import { isAncestor, mergeBase, parseCommit as parseCommitObj, treeFromFiles as buildTreeFromFiles } from './store.ts';

function findRepoByGlobalId(ctx: Ctx, id: string): Repo | null {
  const decoded = ctx.store.decodeGlobalId(id);
  if (decoded === null || decoded.kind !== 'repo') return null;
  const [owner, name] = decoded.key.split('/');
  if (owner === undefined || name === undefined) return null;
  return ctx.store.getRepo(owner, name);
}

function findIssueByGlobalId(ctx: Ctx, id: string): { repo: Repo; issue: Issue } | null {
  const decoded = ctx.store.decodeGlobalId(id);
  if (decoded === null || decoded.kind !== 'issue') return null;
  const match = /^([^/]+)\/([^:]+):(\d+)$/.exec(decoded.key);
  if (match === null) return null;
  const repo = ctx.store.getRepo(match[1]!, match[2]!);
  if (repo === null) return null;
  const issue = repo.issues.find((entry) => entry.number === Number(match[3]));
  return issue === undefined ? null : { repo, issue };
}

function findPullByGlobalId(ctx: Ctx, id: string): { repo: Repo; pull: Pull } | null {
  const decoded = ctx.store.decodeGlobalId(id);
  if (decoded === null || decoded.kind !== 'pull') return null;
  const match = /^([^/]+)\/([^:]+):(\d+)$/.exec(decoded.key);
  if (match === null) return null;
  const repo = ctx.store.getRepo(match[1]!, match[2]!);
  if (repo === null) return null;
  const pull = repo.pulls.find((entry) => entry.number === Number(match[3]));
  return pull === undefined ? null : { repo, pull };
}

/** Squash-merge a pull request; returns the merge commit oid or null on conflict. */
export function squashMerge(store: Store, repo: Repo, pull: Pull): string | null {
  if (pull.merged) return pull.mergeCommitSha;
  // The base tip is read live from the ref, not from the sha captured when
  // the pull was opened: another pull may have merged into the base since.
  // Using the stale sha would compare against the old tree (so a real
  // conflict reads as clean) and parent the squash commit on the old tip.
  const currentBaseSha = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
  const headObj = repo.objects.get(pull.headSha);
  const baseObj = repo.objects.get(currentBaseSha);
  if (headObj === undefined || baseObj === undefined) return null;
  const headTree = parseCommitObj(headObj.data).tree;
  const baseTree = parseCommitObj(baseObj.data).tree;
  const baseFiles = new Map<string, Buffer>();
  flattenTree(repo.objects, baseTree, '', baseFiles);
  const headFiles = new Map<string, Buffer>();
  flattenTree(repo.objects, headTree, '', headFiles);
  if (pull.mergeBase !== null) {
    const mergeCommit = repo.objects.get(pull.mergeBase);
    if (mergeCommit !== undefined) {
      const mergeFiles = new Map<string, Buffer>();
      flattenTree(repo.objects, parseCommitObj(mergeCommit.data).tree, '', mergeFiles);
      for (const [path, headData] of headFiles) {
        const mergeData = mergeFiles.get(path);
        const currentData = baseFiles.get(path);
        if (mergeData !== undefined && currentData !== undefined && !mergeData.equals(headData) && !mergeData.equals(currentData) && !headData.equals(currentData)) {
          return null;
        }
      }
    }
  }
  // The squash commit carries the head tree on top of the current base tip.
  const tree = buildTreeFromFiles(repo.objects, headFiles);
  const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.head}\n\n${pull.title}\n`;
  const stamp = store.nextTimestamp();
  const commitData = Buffer.from(
    `tree ${tree}\nparent ${currentBaseSha}\nauthor ada iversen <adaiversen@example.invalid> ${stamp} +0000\ncommitter ada iversen <adaiversen@example.invalid> ${stamp} +0000\n\n${message}`,
    'utf8',
  );
  const oid = hashOf('commit', commitData);
  repo.objects.set(oid, { type: 'commit', data: commitData });
  repo.setRef(`refs/heads/${pull.base}`, oid);
  pull.baseSha = oid;
  pull.merged = true;
  pull.state = 'closed';
  pull.mergedAt = new Date(stamp * 1000).toISOString();
  pull.mergeCommitSha = oid;
  return oid;
}

import { hashObject as hashOf } from './store.ts';

function executeMutation(ctx: Ctx, sel: Selection): Record<string, unknown> {
  const key = sel.alias ?? sel.name;
  const args = resolveArgs(sel, ctx.variables);
  const input = (args.input ?? {}) as Record<string, unknown>;
  if (sel.name === 'createIssue') {
    const repo = typeof input.repositoryId === 'string' ? findRepoByGlobalId(ctx, input.repositoryId) : null;
    if (repo === null) {
      ctx.errors.push({ message: 'createIssue: repository not found' });
      return { [key]: null };
    }
    repo.issueSeq += 1;
    const issue: Issue = {
      id: 100000 + repo.id * 1000 + repo.issueSeq,
      number: repo.issueSeq,
      title: typeof input.title === 'string' ? input.title : '',
      body: typeof input.body === 'string' ? input.body : '',
      state: 'open',
      labels: [],
      comments: [],
      createdSeq: ctx.store.nextSeq(),
      rev: 0,
    };
    repo.issues.push(issue);
    const out: Record<string, unknown> = {};
    for (const sub of sel.selections) {
      if (sub.name === 'issue') out[sub.alias ?? sub.name] = resolveIssue(ctx, repo, issue, sub.selections);
    }
    return { [key]: out };
  }
  if (sel.name === 'closeIssue') {
    const found = typeof input.issueId === 'string' ? findIssueByGlobalId(ctx, input.issueId) : null;
    if (found === null) {
      ctx.errors.push({ message: 'closeIssue: issue not found' });
      return { [key]: null };
    }
    found.issue.state = 'closed';
    touchIssue(found.issue);
    const out: Record<string, unknown> = {};
    for (const sub of sel.selections) {
      if (sub.name === 'issue') out[sub.alias ?? sub.name] = resolveIssue(ctx, found.repo, found.issue, sub.selections);
    }
    return { [key]: out };
  }
  if (sel.name === 'createPullRequest') {
    const repo = typeof input.repositoryId === 'string' ? findRepoByGlobalId(ctx, input.repositoryId) : null;
    if (repo === null) {
      ctx.errors.push({ message: 'createPullRequest: repository not found' });
      return { [key]: null };
    }
    const head = typeof input.headRefName === 'string' ? input.headRefName : '';
    const base = typeof input.baseRefName === 'string' ? input.baseRefName : repo.defaultBranch;
    const headSha = repo.branchTip(head) ?? '';
    const baseSha = repo.branchTip(base) ?? '';
    repo.pullSeq += 1;
    const pull: Pull = {
      id: 200000 + repo.id * 1000 + repo.pullSeq,
      number: repo.pullSeq,
      title: typeof input.title === 'string' ? input.title : '',
      body: typeof input.body === 'string' ? input.body : '',
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      head,
      base,
      headSha,
      baseSha,
      mergeBase: headSha !== '' && baseSha !== '' ? mergeBase(repo.objects, headSha, baseSha) : null,
      createdSeq: ctx.store.nextSeq(),
    };
    repo.pulls.push(pull);
    const out: Record<string, unknown> = {};
    for (const sub of sel.selections) {
      if (sub.name === 'pullRequest') out[sub.alias ?? sub.name] = resolvePull(ctx, repo, pull, sub.selections);
    }
    return { [key]: out };
  }
  if (sel.name === 'mergePullRequest') {
    const found = typeof input.pullRequestId === 'string' ? findPullByGlobalId(ctx, input.pullRequestId) : null;
    if (found === null) {
      ctx.errors.push({ message: 'mergePullRequest: pull request not found' });
      return { [key]: null };
    }
    squashMerge(ctx.store, found.repo, found.pull);
    const out: Record<string, unknown> = {};
    for (const sub of sel.selections) {
      if (sub.name === 'pullRequest') out[sub.alias ?? sub.name] = resolvePull(ctx, found.repo, found.pull, sub.selections);
    }
    return { [key]: out };
  }
  if (sel.name === 'addComment') {
    const found = typeof input.subjectId === 'string' ? findIssueByGlobalId(ctx, input.subjectId) : null;
    if (found === null) {
      ctx.errors.push({ message: 'addComment: subject not found' });
      return { [key]: null };
    }
    found.repo.commentSeq += 1;
    const comment = { id: found.repo.commentSeq, body: typeof input.body === 'string' ? input.body : '' };
    found.issue.comments.push(comment);
    touchIssue(found.issue);
    const node = { id: ctx.store.globalId('comment', `${found.repo.owner}/${found.repo.name}:${found.issue.number}:${comment.id}`), body: comment.body };
    const out: Record<string, unknown> = {};
    for (const sub of sel.selections) {
      if (sub.name === 'commentEdge') {
        const edge: Record<string, unknown> = {};
        for (const edgeSel of sub.selections) {
          if (edgeSel.name === 'node') {
            const nodeOut: Record<string, unknown> = {};
            for (const nodeSel of edgeSel.selections) {
              const nodeKey = nodeSel.alias ?? nodeSel.name;
              if (nodeSel.name === 'id') nodeOut[nodeKey] = node.id;
              else if (nodeSel.name === 'body') nodeOut[nodeKey] = node.body;
            }
            edge[edgeSel.alias ?? edgeSel.name] = nodeOut;
          }
        }
        out[sub.alias ?? sub.name] = edge;
      }
    }
    return { [key]: out };
  }
  ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "Mutation".` });
  return { [key]: null };
}

function executeQuery(ctx: Ctx, sel: Selection): Record<string, unknown> {
  const key = sel.alias ?? sel.name;
  const args = resolveArgs(sel, ctx.variables);
  if (sel.name === '__type') {
    const name = typeof args.name === 'string' ? args.name : '';
    const type = resolveType(ctx, name, sel.selections);
    return { [key]: type };
  }
  if (sel.name === 'viewer') {
    const out: Record<string, unknown> = {};
    for (const sub of sel.selections) {
      const subKey = sub.alias ?? sub.name;
      if (sub.name === 'login') out[subKey] = VIEWER_LOGIN;
      else if (sub.name === 'id') out[subKey] = VIEWER_ID;
      else if (sub.name === 'name') out[subKey] = VIEWER_NAME;
      else fieldError(ctx, 'User', sub.name);
    }
    return { [key]: out };
  }
  if (sel.name === 'repository') {
    const owner = typeof args.owner === 'string' ? args.owner : '';
    const name = typeof args.name === 'string' ? args.name : '';
    const repo = ctx.store.getRepo(owner, name);
    if (repo === null) return { [key]: null };
    return { [key]: resolveRepository(ctx, repo, sel.selections) };
  }
  if (sel.name === 'node') {
    const id = typeof args.id === 'string' ? args.id : '';
    const repo = findRepoByGlobalId(ctx, id);
    if (repo === null) return { [key]: null };
    const out = resolveRepository(ctx, repo, sel.selections.filter((entry) => entry.name !== '__fragment'));
    for (const fragment of sel.selections.filter((entry) => entry.name === '__fragment')) {
      if (fragment.fragmentType === 'Repository') Object.assign(out, resolveRepository(ctx, repo, fragment.selections));
    }
    if (sel.selections.some((entry) => entry.name === 'id')) out.id = id;
    return { [key]: out };
  }
  ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "Query".` });
  return { [key]: null };
}

export function executeGraphQL(store: Store, query: string, variables: Record<string, unknown>): { data: Record<string, unknown> | null; errors: Array<{ message: string }> } {
  const ctx: Ctx = { store, variables, errors: [] };
  let operations: Operation[];
  try {
    operations = new Parser(tokenize(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
  }
  const data: Record<string, unknown> = {};
  for (const operation of operations) {
    for (const sel of operation.selections) {
      if (sel.name === '__fragment') continue;
      const result = operation.type === 'mutation' ? executeMutation(ctx, sel) : executeQuery(ctx, sel);
      Object.assign(data, result);
    }
  }
  return { data, errors: ctx.errors };
}

export { commitDistances, isAncestor };
