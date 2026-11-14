// The forge's HTTP server: health probe, REST v3, GraphQL v4, MCP and git
// smart HTTP, all on one in-memory store. Every REST reply is JSON with a
// non-empty X-GitHub-Request-Id; unknown routes are JSON 404s; a malformed
// JSON body is a 400.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import {
  DEFAULT_BRANCH,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  hashObject,
  issueJson,
  lineCounts,
  pullJson,
  repoJson,
  type Repo,
  type Store,
} from './store.ts';
import { Store as StoreClass } from './store.ts';
import { flattenTree, parseCommit, touchIssue, treeFromFiles } from './store.ts';
import { infoRefs, receivePack, uploadPack } from './git.ts';
import { executeGraphQL, squashMerge } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { mergeBase } from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const STORE = new StoreClass();
let requestSeq = 0;

function nextRequestId(): string {
  requestSeq += 1;
  return `req-${requestSeq}`;
}

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  const text = body === null ? '' : `${JSON.stringify(body)}\n`;
  // An explicit content-length keeps Node from falling back to chunked framing:
  // one writev, and the client knows the body size up front.
  res.writeHead(status, { 'content-type': JSON_TYPE, 'x-github-request-id': nextRequestId(), 'content-length': Buffer.byteLength(text), ...headers });
  res.end(text);
}

function sendNoContent(res: ServerResponse): void {
  res.writeHead(204, { 'x-github-request-id': nextRequestId(), 'content-length': 0 });
  res.end();
}

function sendError(res: ServerResponse, status: number, message: string): void {
  sendJson(res, status, { message });
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on('data', (chunk: Buffer) => {
      chunks.push(chunk);
      size += chunk.length;
      if (size > 64 * 1024 * 1024) {
        reject(new Error('body too large'));
      }
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

interface ParsedUrl {
  pathname: string;
  segments: string[];
  query: URLSearchParams;
}

function parseUrl(req: IncomingMessage): ParsedUrl {
  const raw = req.url ?? '/';
  const q = raw.indexOf('?');
  const pathname = q === -1 ? raw : raw.slice(0, q);
  // Dot segments are the one case the fast split mishandles; fall back to URL.
  if (pathname.includes('/./') || pathname.includes('/../') || pathname.endsWith('/.') || pathname.endsWith('/..')) {
    try {
      const url = new URL(raw, 'http://entry');
      return {
        pathname: url.pathname,
        segments: url.pathname.split('/').filter((part) => part !== '').map((part) => {
          try {
            return decodeURIComponent(part);
          } catch {
            return part;
          }
        }),
        query: url.searchParams,
      };
    } catch {
      return { pathname: '/', segments: [], query: new URLSearchParams() };
    }
  }
  const segments = pathname.split('/').filter((part) => part !== '').map((part) => {
    if (!part.includes('%')) return part;
    try {
      return decodeURIComponent(part);
    } catch {
      return part;
    }
  });
  let query: URLSearchParams;
  try {
    query = q === -1 ? new URLSearchParams() : new URLSearchParams(raw.slice(q + 1));
  } catch {
    query = new URLSearchParams();
  }
  return { pathname, segments, query };
}

/** The bearer token never changes during the process: build its header once. */
let expectedAuthHeader: string | null = null;

function authorized(req: IncomingMessage): boolean {
  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  if (expected === '') return true;
  if (expectedAuthHeader === null) expectedAuthHeader = `Bearer ${expected}`;
  const header = req.headers.authorization ?? '';
  return header === expectedAuthHeader;
}

/**
 * An issue's serialized JSON, cached until the issue mutates. issueJson rebuilds
 * node_id (base64), two ISO dates and the whole object on every read; the read
 * workload GETs the same issue 2,000 times, and everything in the JSON except
 * title/body/state/labels/comments-count is fixed at creation. The revision lives
 * on the issue itself (store.ts `rev`, bumped by `touchIssue` at every mutation
 * site in REST, GraphQL and MCP), so a stale entry is impossible while it matches.
 */
const issueJsonCache = new WeakMap<object, { rev: number; text: string }>();

function issueJsonText(repo: Repo, issue: Parameters<typeof issueJson>[1]): string {
  const cached = issueJsonCache.get(issue);
  if (cached !== undefined && cached.rev === issue.rev) return cached.text;
  const text = `${JSON.stringify(issueJson(repo, issue))}\n`;
  issueJsonCache.set(issue, { rev: issue.rev, text });
  return text;
}

function sendJsonText(res: ServerResponse, status: number, text: string): void {
  res.writeHead(status, { 'content-type': JSON_TYPE, 'x-github-request-id': nextRequestId(), 'content-length': Buffer.byteLength(text) });
  res.end(text);
}

function parseJsonBody(raw: Buffer): { ok: true; value: Record<string, unknown> } | { ok: false } {
  if (raw.length === 0) return { ok: true, value: {} };
  try {
    const value = JSON.parse(raw.toString('utf8')) as unknown;
    if (value !== null && typeof value === 'object' && !Array.isArray(value)) return { ok: true, value: value as Record<string, unknown> };
    return { ok: true, value: {} };
  } catch {
    return { ok: false };
  }
}

function paginate<T>(items: readonly T[], query: URLSearchParams): { page: T[]; hasNext: boolean; pageNum: number; perPage: number } {
  const perPage = Math.max(1, Math.min(100, Number.parseInt(query.get('per_page') ?? '30', 10) || 30));
  const pageNum = Math.max(1, Number.parseInt(query.get('page') ?? '1', 10) || 1);
  const start = (pageNum - 1) * perPage;
  return { page: items.slice(start, start + perPage) as T[], hasNext: start + perPage < items.length, pageNum, perPage };
}

function linkHeader(req: IncomingMessage, path: string, query: URLSearchParams, pageNum: number, perPage: number): string {
  const host = req.headers.host ?? 'localhost';
  const params = new URLSearchParams(query);
  params.set('page', String(pageNum + 1));
  params.set('per_page', String(perPage));
  return `<http://${host}${path}?${params.toString()}>; rel="next"`;
}

function repoFromSegments(store: Store, segments: string[]): Repo | null {
  // segments: ['api','v3','repos', owner, repo, ...]
  const owner = segments[3] ?? '';
  const name = segments[4] ?? '';
  return store.getRepo(owner, name);
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asBoolean(value: unknown, fallback: boolean): boolean {
  return typeof value === 'boolean' ? value : fallback;
}

// -------------------------------------------------------------------------------------------
// Contents
// -------------------------------------------------------------------------------------------

function resolveRefTip(repo: Repo, ref: string | null): string | undefined {
  if (ref === null || ref === '') return repo.branchTip(repo.defaultBranch);
  const branchTip = repo.branchTip(ref);
  if (branchTip !== undefined) return branchTip;
  if (/^[0-9a-f]{40}$/.test(ref)) return repo.objects.has(ref) ? ref : undefined;
  return undefined;
}

function snapshotAtRef(repo: Repo, ref: string | null): Map<string, Buffer> | null {
  const tip = resolveRefTip(repo, ref);
  if (tip === undefined) return null;
  const obj = repo.objects.get(tip);
  if (obj === undefined || obj.type !== 'commit') return null;
  const snapshot = new Map<string, Buffer>();
  flattenTree(repo.objects, parseCommit(obj.data).tree, '', snapshot);
  return snapshot;
}

function subtreeTreeOid(repo: Repo, files: ReadonlyMap<string, Buffer>, prefix: string): string {
  const sub = new Map<string, Buffer>();
  for (const [path, data] of files) {
    if (path.startsWith(`${prefix}/`)) sub.set(path.slice(prefix.length + 1), data);
  }
  return treeFromFiles(repo.objects, sub);
}

function handleContentsGet(req: IncomingMessage, res: ServerResponse, repo: Repo, pathParts: string[], query: URLSearchParams): void {
  const path = pathParts.join('/');
  const ref = query.get('ref');
  const snapshot = snapshotAtRef(repo, ref);
  if (snapshot === null) {
    sendError(res, 404, 'Not Found');
    return;
  }
  if (path === '') {
    // Root directory listing.
    const entries = dirEntries(repo, snapshot, '');
    sendJson(res, 200, entries);
    return;
  }
  const data = snapshot.get(path);
  if (data !== undefined) {
    const sha = hashObject('blob', data);
    sendJson(res, 200, {
      type: 'file',
      encoding: 'base64',
      size: data.length,
      name: path.split('/').pop() ?? path,
      path,
      content: data.toString('base64'),
      sha,
      url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${path}`,
    });
    return;
  }
  const prefix = `${path}/`;
  const hasChildren = [...snapshot.keys()].some((key) => key.startsWith(prefix));
  if (!hasChildren) {
    sendError(res, 404, 'Not Found');
    return;
  }
  sendJson(res, 200, dirEntries(repo, snapshot, path));
}

function dirEntries(repo: Repo, snapshot: ReadonlyMap<string, Buffer>, dir: string): Array<Record<string, unknown>> {
  const prefix = dir === '' ? '' : `${dir}/`;
  const files = new Map<string, Buffer>();
  const dirs = new Set<string>();
  for (const [path, data] of snapshot) {
    if (!path.startsWith(prefix)) continue;
    const rest = path.slice(prefix.length);
    const slash = rest.indexOf('/');
    if (slash === -1) files.set(rest, data);
    else dirs.add(rest.slice(0, slash));
  }
  const entries: Array<Record<string, unknown>> = [];
  for (const name of [...dirs].sort()) {
    const subPath = prefix + name;
    entries.push({ type: 'dir', name, path: subPath, sha: subtreeTreeOid(repo, snapshot, subPath), url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${subPath}` });
  }
  for (const [name, data] of [...files.entries()].sort(([a], [b]) => (a < b ? -1 : 1))) {
    const subPath = prefix + name;
    entries.push({ type: 'file', name, path: subPath, sha: hashObject('blob', data), size: data.length, url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${subPath}` });
  }
  return entries;
}

function handleContentsPut(res: ServerResponse, repo: Repo, pathParts: string[], body: Record<string, unknown>): void {
  const path = pathParts.join('/');
  if (path === '') {
    sendError(res, 400, 'a path is required');
    return;
  }
  const branch = asString(body.branch) || repo.defaultBranch;
  let tip = repo.branchTip(branch);
  if (tip === undefined) {
    const defaultTip = repo.branchTip(repo.defaultBranch);
    if (defaultTip !== undefined) {
      repo.refs.set(`refs/heads/${branch}`, defaultTip);
      repo.setRef(`refs/heads/${branch}`, defaultTip);
      tip = defaultTip;
    }
  }
  const content = typeof body.content === 'string' ? Buffer.from(body.content.replace(/[\r\n\t ]+/g, ''), 'base64') : Buffer.alloc(0);
  const snapshot = repo.branchFiles(branch);
  const existed = snapshot.has(path);
  if (existed && typeof body.sha === 'string' && body.sha !== '') {
    const currentSha = hashObject('blob', snapshot.get(path)!);
    if (body.sha.toLowerCase() !== currentSha.toLowerCase()) {
      sendError(res, 409, 'sha does not match the current blob');
      return;
    }
  }
  snapshot.set(path, content);
  const message = asString(body.message) || `Update ${path}`;
  const commitSha = repo.commitBranch(STORE, branch, message);
  const blobSha = hashObject('blob', content);
  sendJson(res, existed ? 200 : 201, {
    content: { name: path.split('/').pop() ?? path, path, sha: blobSha, size: content.length, type: 'file', encoding: 'base64', content: content.toString('base64') },
    commit: { sha: commitSha, message },
  });
}

function handleContentsDelete(res: ServerResponse, repo: Repo, pathParts: string[], body: Record<string, unknown>): void {
  const path = pathParts.join('/');
  const branch = asString(body.branch) || repo.defaultBranch;
  const snapshot = repo.branchFiles(branch);
  if (!snapshot.has(path)) {
    sendError(res, 404, 'Not Found');
    return;
  }
  snapshot.delete(path);
  const message = asString(body.message) || `Delete ${path}`;
  const commitSha = repo.commitBranch(STORE, branch, message);
  sendJson(res, 200, { commit: { sha: commitSha, message } });
}

// -------------------------------------------------------------------------------------------
// Issues, pulls, branches, commits
// -------------------------------------------------------------------------------------------

function labelNames(body: Record<string, unknown>): string[] {
  const raw = body.labels;
  if (!Array.isArray(raw)) return [];
  const names: string[] = [];
  for (const entry of raw) {
    if (typeof entry === 'string') names.push(entry);
    else if (entry !== null && typeof entry === 'object' && typeof (entry as Record<string, unknown>).name === 'string') {
      names.push((entry as Record<string, unknown>).name as string);
    }
  }
  return names;
}

function handleIssues(req: IncomingMessage, res: ServerResponse, repo: Repo, rest: string[], query: URLSearchParams, method: string, body: Record<string, unknown>): void {
  if (rest.length === 0) {
    if (method === 'GET') {
      const state = (query.get('state') ?? 'open').toLowerCase();
      const sort = query.get('sort') ?? 'created';
      const direction = (query.get('direction') ?? 'desc').toLowerCase();
      let issues = [...repo.issues];
      if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
      if (sort === 'created') {
        issues.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
      }
      const { page } = paginate(issues, query);
      sendJson(res, 200, page.map((issue) => issueJson(repo, issue)));
      return;
    }
    if (method === 'POST') {
      const names = labelNames(body);
      for (const name of names) {
        if (!repo.labels.has(name)) {
          sendError(res, 422, `label ${name} does not exist`);
          return;
        }
      }
      repo.issueSeq += 1;
      const issue = {
        id: 100000 + repo.id * 1000 + repo.issueSeq,
        number: repo.issueSeq,
        title: asString(body.title),
        body: asString(body.body),
        state: 'open' as const,
        labels: names.map((name) => repo.labels.get(name)!),
        comments: [] as Array<{ id: number; body: string }>,
        createdSeq: STORE.nextSeq(),
        rev: 0,
      };
      repo.issues.push(issue);
      sendJsonText(res, 201, issueJsonText(repo, issue));
      return;
    }
    sendError(res, 404, 'Not Found');
    return;
  }
  const number = Number(rest[0]);
  if (!Number.isInteger(number)) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const issue = repo.issues.find((entry) => entry.number === number);
  if (rest.length === 2 && rest[1] === 'comments') {
    if (issue === undefined) {
      sendError(res, 404, 'Not Found');
      return;
    }
    if (method === 'GET') {
      sendJson(res, 200, issue.comments.map((comment) => ({ id: comment.id, body: comment.body, user: { login: VIEWER_LOGIN, id: VIEWER_ID } })));
      return;
    }
    if (method === 'POST') {
      repo.commentSeq += 1;
      const comment = { id: repo.commentSeq, body: asString(body.body) };
      issue.comments.push(comment);
      touchIssue(issue);
      sendJson(res, 201, { id: comment.id, body: comment.body, user: { login: VIEWER_LOGIN, id: VIEWER_ID } });
      return;
    }
    sendError(res, 404, 'Not Found');
    return;
  }
  if (rest.length !== 1) {
    sendError(res, 404, 'Not Found');
    return;
  }
  if (issue === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  if (method === 'GET') {
    sendJsonText(res, 200, issueJsonText(repo, issue));
    return;
  }
  if (method === 'PATCH') {
    if (typeof body.state === 'string' && (body.state === 'open' || body.state === 'closed')) issue.state = body.state;
    if (typeof body.title === 'string') issue.title = body.title;
    if (typeof body.body === 'string') issue.body = body.body;
    touchIssue(issue);
    sendJsonText(res, 200, issueJsonText(repo, issue));
    return;
  }
  sendError(res, 404, 'Not Found');
}

function pullFilesList(repo: Repo, pull: { headSha: string; baseSha: string; mergeBase: string | null }): Array<Record<string, unknown>> {
  const headObj = repo.objects.get(pull.headSha);
  if (headObj === undefined) return [];
  const headTree = parseCommit(headObj.data).tree;
  let baseTree: string | undefined;
  if (pull.mergeBase !== null) {
    const baseCommit = repo.objects.get(pull.mergeBase);
    if (baseCommit !== undefined) baseTree = parseCommit(baseCommit.data).tree;
  }
  const diff = repo.diffTrees(baseTree, headTree);
  return diff
    .filter((entry) => entry.status !== 'removed')
    .map((entry) => {
      const counts = lineCounts(entry.old, entry.current);
      return {
        filename: entry.path,
        status: entry.status === 'added' ? 'added' : 'modified',
        additions: counts.additions,
        deletions: counts.deletions,
        changes: counts.additions + counts.deletions,
        sha: hashObject('blob', entry.current),
      };
    });
}

function handlePulls(req: IncomingMessage, res: ServerResponse, repo: Repo, rest: string[], query: URLSearchParams, method: string, body: Record<string, unknown>): void {
  if (rest.length === 0) {
    if (method === 'GET') {
      const state = (query.get('state') ?? 'open').toLowerCase();
      let pulls = [...repo.pulls];
      if (state !== 'all') pulls = pulls.filter((pull) => pull.state === state);
      const { page } = paginate(pulls, query);
      sendJson(res, 200, page.map((pull) => pullJson(repo, pull)));
      return;
    }
    if (method === 'POST') {
      const headRaw = asString(body.head);
      const baseRaw = asString(body.base);
      const head = headRaw.includes(':') ? headRaw.split(':').pop()! : headRaw;
      const base = baseRaw.includes(':') ? baseRaw.split(':').pop()! : baseRaw;
      const headSha = repo.branchTip(head) ?? '';
      const baseSha = repo.branchTip(base) ?? '';
      if (headSha === '' || baseSha === '') {
        sendError(res, 422, 'head or base branch not found');
        return;
      }
      repo.pullSeq += 1;
      const pull = {
        id: 200000 + repo.id * 1000 + repo.pullSeq,
        number: repo.pullSeq,
        title: asString(body.title),
        body: asString(body.body),
        state: 'open' as const,
        merged: false,
        mergedAt: null as string | null,
        mergeCommitSha: null as string | null,
        head,
        base,
        headSha,
        baseSha,
        mergeBase: mergeBase(repo.objects, headSha, baseSha),
        createdSeq: STORE.nextSeq(),
      };
      repo.pulls.push(pull);
      sendJson(res, 201, pullJson(repo, pull));
      return;
    }
    sendError(res, 404, 'Not Found');
    return;
  }
  const number = Number(rest[0]);
  if (!Number.isInteger(number)) {
    sendError(res, 404, 'Not Found');
    return;
  }
  const pull = repo.pulls.find((entry) => entry.number === number);
  if (rest.length === 2 && rest[1] === 'files') {
    if (pull === undefined) {
      sendError(res, 404, 'Not Found');
      return;
    }
    sendJson(res, 200, pullFilesList(repo, pull));
    return;
  }
  if (rest.length === 2 && rest[1] === 'merge') {
    if (pull === undefined) {
      sendError(res, 404, 'Not Found');
      return;
    }
    if (method !== 'PUT') {
      sendError(res, 404, 'Not Found');
      return;
    }
    if (pull.merged) {
      sendJson(res, 200, { sha: pull.mergeCommitSha, merged: true, message: 'already merged' });
      return;
    }
    const merged = squashMerge(STORE, repo, pull);
    if (merged === null) {
      sendError(res, 405, 'merge conflict');
      return;
    }
    sendJson(res, 200, { sha: merged, merged: true, message: `Pull request #${pull.number} merged` });
    return;
  }
  if (rest.length !== 1) {
    sendError(res, 404, 'Not Found');
    return;
  }
  if (pull === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  if (method === 'GET') {
    sendJson(res, 200, pullJson(repo, pull));
    return;
  }
  sendError(res, 404, 'Not Found');
}

function handleBranches(res: ServerResponse, repo: Repo, rest: string[], method: string): void {
  if (method !== 'GET') {
    sendError(res, 404, 'Not Found');
    return;
  }
  if (rest.length === 0) {
    const branches: Array<Record<string, unknown>> = [];
    for (const [ref, oid] of repo.refs) {
      if (!ref.startsWith('refs/heads/')) continue;
      branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha: oid }, protected: false });
    }
    branches.sort((a, b) => String(a.name) < String(b.name) ? -1 : 1);
    sendJson(res, 200, branches);
    return;
  }
  const name = rest.join('/');
  const tip = repo.branchTip(name);
  if (tip === undefined) {
    sendError(res, 404, 'Not Found');
    return;
  }
  sendJson(res, 200, { name, commit: { sha: tip }, protected: false });
}

function handleCommits(res: ServerResponse, repo: Repo, query: URLSearchParams): void {
  const shaParam = query.get('sha');
  let tip: string | undefined;
  if (shaParam === null || shaParam === '') tip = repo.branchTip(repo.defaultBranch);
  else tip = repo.branchTip(shaParam) ?? (/^[0-9a-f]{40}$/.test(shaParam) ? shaParam : undefined);
  if (tip === undefined) {
    sendJson(res, 200, []);
    return;
  }
  const commits = repo.history(tip).map((oid) => {
    const commit = parseCommit(repo.objects.get(oid)!.data);
    const author = commit.author.replace(/ <.*$/, '');
    const emailMatch = /<([^>]*)>/.exec(commit.author);
    return {
      sha: oid,
      commit: {
        // GitHub's API trims the trailing newline the git object carries.
        message: commit.message.replace(/\n+$/, ''),
        author: { name: author, email: emailMatch?.[1] ?? VIEWER_EMAIL, date: new Date(commit.timestamp * 1000).toISOString() },
        committer: { name: author, email: emailMatch?.[1] ?? VIEWER_EMAIL, date: new Date(commit.timestamp * 1000).toISOString() },
        tree: { sha: commit.tree },
      },
      author: { login: VIEWER_LOGIN, id: VIEWER_ID },
      committer: { login: VIEWER_LOGIN, id: VIEWER_ID },
    };
  });
  sendJson(res, 200, commits);
}

function handleGitRefs(res: ServerResponse, repo: Repo, rest: string[], method: string, body: Record<string, unknown>): void {
  if (method === 'POST' && rest.length === 0) {
    const ref = asString(body.ref);
    const sha = asString(body.sha);
    if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha) || !repo.objects.has(sha)) {
      sendError(res, 422, 'invalid ref or sha');
      return;
    }
    repo.setRef(ref, sha);
    sendJson(res, 201, { ref, object: { sha }, url: `/api/v3/repos/${repo.owner}/${repo.name}/git/refs/${ref}` });
    return;
  }
  if (method === 'GET') {
    if (rest.length === 0) {
      const refs = [...repo.refs.entries()].map(([ref, oid]) => ({ ref, object: { sha: oid } }));
      sendJson(res, 200, refs);
      return;
    }
    const ref = `refs/${rest.join('/')}`;
    const oid = repo.refs.get(ref);
    if (oid === undefined) {
      sendError(res, 404, 'Not Found');
      return;
    }
    sendJson(res, 200, { ref, object: { sha: oid } });
    return;
  }
  sendError(res, 404, 'Not Found');
}

// -------------------------------------------------------------------------------------------
// REST dispatcher
// -------------------------------------------------------------------------------------------

async function handleRest(req: IncomingMessage, res: ServerResponse, segments: string[], query: URLSearchParams, method: string, rawBody: Buffer): Promise<void> {
  // API root and meta are public, like GitHub's; everything else needs the token.
  const isRoot = segments.length === 2 || (segments.length === 3 && segments[2] === '');
  const isMeta = segments.length === 3 && segments[2] === 'meta';
  if (!isRoot && !isMeta && !authorized(req)) {
    sendError(res, 401, 'Bad credentials');
    return;
  }
  if (isRoot && method === 'GET') {
    sendJson(res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    return;
  }
  if (isMeta && method === 'GET') {
    sendJson(res, 200, { installed_version: '1.0.0', verifiable_password_authentication: true });
    return;
  }
  // GET /api/v3/user
  if (segments.length === 3 && segments[2] === 'user' && method === 'GET') {
    sendJson(res, 200, { login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL, type: 'User' });
    return;
  }
  // /api/v3/user/repos
  if (segments.length === 4 && segments[2] === 'user' && segments[3] === 'repos') {
    if (method === 'GET') {
      const repos = STORE.listRepos();
      const { page, hasNext, pageNum, perPage } = paginate(repos, query);
      const headers: Record<string, string> = {};
      if (hasNext) headers.link = linkHeader(req, '/api/v3/user/repos', query, pageNum, perPage);
      sendJson(res, 200, page.map((repo) => repoJson(repo)), headers);
      return;
    }
    if (method === 'POST') {
      const parsed = parseJsonBody(rawBody);
      if (!parsed.ok) {
        sendError(res, 400, 'Invalid JSON');
        return;
      }
      const name = asString(parsed.value.name);
      if (name === '') {
        sendError(res, 422, 'name is required');
        return;
      }
      const repo = STORE.createRepo(name, asBoolean(parsed.value.private, false), asString(parsed.value.description));
      if (repo === null) {
        sendError(res, 422, 'Repository already exists');
        return;
      }
      sendJson(res, 201, repoJson(repo));
      return;
    }
    sendError(res, 404, 'Not Found');
    return;
  }
  // /api/v3/repos/{owner}/{repo}[...]
  if (segments.length >= 5 && segments[2] === 'repos') {
    const repo = repoFromSegments(STORE, segments);
    if (repo === null) {
      sendError(res, 404, 'Not Found');
      return;
    }
    const rest = segments.slice(5);
    if (rest.length === 0) {
      if (method === 'GET') {
        sendJson(res, 200, repoJson(repo));
        return;
      }
      if (method === 'PATCH') {
        const parsed = parseJsonBody(rawBody);
        if (!parsed.ok) {
          sendError(res, 400, 'Invalid JSON');
          return;
        }
        if (typeof parsed.value.description === 'string') repo.description = parsed.value.description;
        if (typeof parsed.value.private === 'boolean') repo.privateFlag = parsed.value.private;
        sendJson(res, 200, repoJson(repo));
        return;
      }
      if (method === 'DELETE') {
        STORE.repos.delete(repo.key);
        sendNoContent(res);
        return;
      }
      sendError(res, 404, 'Not Found');
      return;
    }
    const resource = rest[0]!;
    if (resource === 'contents') {
      if (method === 'GET') {
        handleContentsGet(req, res, repo, rest.slice(1), query);
        return;
      }
      const parsed = parseJsonBody(rawBody);
      if (!parsed.ok) {
        sendError(res, 400, 'Invalid JSON');
        return;
      }
      if (method === 'PUT') {
        handleContentsPut(res, repo, rest.slice(1), parsed.value);
        return;
      }
      if (method === 'DELETE') {
        handleContentsDelete(res, repo, rest.slice(1), parsed.value);
        return;
      }
      sendError(res, 404, 'Not Found');
      return;
    }
    if (resource === 'labels') {
      if (method === 'GET') {
        sendJson(res, 200, [...repo.labels.values()].map((label) => ({ name: label.name, color: label.color })));
        return;
      }
      if (method === 'POST') {
        const parsed = parseJsonBody(rawBody);
        if (!parsed.ok) {
          sendError(res, 400, 'Invalid JSON');
          return;
        }
        const name = asString(parsed.value.name);
        if (name === '') {
          sendError(res, 422, 'name is required');
          return;
        }
        const label = { name, color: asString(parsed.value.color) || 'ededed' };
        repo.labels.set(name, label);
        sendJson(res, 201, { name: label.name, color: label.color, id: 1000 + repo.labels.size });
        return;
      }
      sendError(res, 404, 'Not Found');
      return;
    }
    if (resource === 'issues') {
      const parsed = method === 'GET' ? { ok: true as const, value: {} } : parseJsonBody(rawBody);
      if (!parsed.ok) {
        sendError(res, 400, 'Invalid JSON');
        return;
      }
      handleIssues(req, res, repo, rest.slice(1), query, method, parsed.value);
      return;
    }
    if (resource === 'pulls') {
      const parsed = method === 'GET' ? { ok: true as const, value: {} } : parseJsonBody(rawBody);
      if (!parsed.ok) {
        sendError(res, 400, 'Invalid JSON');
        return;
      }
      handlePulls(req, res, repo, rest.slice(1), query, method, parsed.value);
      return;
    }
    if (resource === 'branches') {
      handleBranches(res, repo, rest.slice(1), method);
      return;
    }
    if (resource === 'commits') {
      if (method === 'GET') {
        handleCommits(res, repo, query);
        return;
      }
      sendError(res, 404, 'Not Found');
      return;
    }
    if (resource === 'git' && rest[1] === 'refs') {
      const parsed = method === 'GET' ? { ok: true as const, value: {} } : parseJsonBody(rawBody);
      if (!parsed.ok) {
        sendError(res, 400, 'Invalid JSON');
        return;
      }
      handleGitRefs(res, repo, rest.slice(2), method, parsed.value);
      return;
    }
    sendError(res, 404, 'Not Found');
    return;
  }
  sendError(res, 404, 'Not Found');
}

// -------------------------------------------------------------------------------------------
// Top-level routing
// -------------------------------------------------------------------------------------------

function gitResponse(res: ServerResponse, result: { status: number; contentType: string; body: Buffer }): void {
  res.writeHead(result.status, { 'content-type': result.contentType, 'x-github-request-id': nextRequestId(), 'content-length': result.body.length });
  res.end(result.body);
}

export function createApp(): Server {
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      const method = req.method ?? 'GET';
      const { pathname, segments, query } = parseUrl(req);
      try {
        if (method === 'GET' && pathname === '/healthz') {
          sendJson(res, 200, { ok: true });
          return;
        }
        // Git smart HTTP: /{owner}/{repo}.git/...
        if (segments.length >= 2 && segments[1]!.endsWith('.git')) {
          const owner = segments[0]!;
          const repoName = segments[1]!.slice(0, -4);
          const repo = STORE.getRepo(owner, repoName);
          const tail = segments.slice(2);
          if (tail.length === 2 && tail[0] === 'info' && tail[1] === 'refs' && method === 'GET') {
            const service = query.get('service') ?? '';
            const wantV2 = (req.headers['git-protocol'] ?? '') === 'version=2' && service === 'git-upload-pack';
            gitResponse(res, infoRefs(repo, service, wantV2));
            return;
          }
          if (tail.length === 1 && tail[0] === 'git-upload-pack' && method === 'POST') {
            const raw = await readBody(req);
            const wantV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
            gitResponse(res, uploadPack(repo, raw, wantV2));
            return;
          }
          if (tail.length === 1 && tail[0] === 'git-receive-pack' && method === 'POST') {
            const raw = await readBody(req);
            gitResponse(res, receivePack(repo, raw));
            return;
          }
          sendError(res, 404, 'Not Found');
          return;
        }
        // GraphQL v4
        if (pathname === '/api/graphql' && method === 'POST') {
          if (!authorized(req)) {
            sendError(res, 401, 'Bad credentials');
            return;
          }
          const raw = await readBody(req);
          let parsed: unknown;
          try {
            parsed = JSON.parse(raw.toString('utf8'));
          } catch {
            sendJson(res, 200, { data: null, errors: [{ message: 'Syntax Error: Unexpected token' }] });
            return;
          }
          const operations = Array.isArray(parsed) ? parsed : [parsed];
          const results = operations.map((operation) => {
            const op = operation as Record<string, unknown>;
            const queryText = typeof op.query === 'string' ? op.query : '';
            const variables = (op.variables ?? {}) as Record<string, unknown>;
            return executeGraphQL(STORE, queryText, variables);
          });
          sendJson(res, 200, Array.isArray(parsed) ? results : results[0]);
          return;
        }
        // MCP
        if (pathname === '/mcp' && method === 'POST') {
          const raw = await readBody(req);
          const outcome = handleMcp(STORE, raw, authorized(req));
          res.writeHead(outcome.status, { ...outcome.headers, 'x-github-request-id': nextRequestId(), 'content-length': outcome.body.length });
          res.end(outcome.body);
          return;
        }
        // REST v3
        if (segments.length >= 2 && segments[0] === 'api' && segments[1] === 'v3') {
          const raw = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);
          await handleRest(req, res, segments, query, method, raw);
          return;
        }
        sendError(res, 404, 'Not Found');
      } catch (err) {
        if (!res.headersSent) sendError(res, 500, err instanceof Error ? err.message : String(err));
        else res.end();
      }
    })();
  });
}
