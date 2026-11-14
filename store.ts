// The forge's in-memory store: git objects, refs, branch file snapshots, issues,
// pull requests and labels. Everything lives in Maps; nothing touches the disk,
// so the read and write workloads stay fast and the trial stays deterministic
// (timestamps come from a counter, never the wall clock).
//
// The scenario the suite drives is one fixed seed (wire.ts SCENARIO_SEED), so the
// token's owner is the same login in every trial: adaiversen, id 1000. The bearer
// token itself is opaque (shpat_<hex>), so the owner is fixed here rather than
// decoded from it; any other owner name on a repo URL is a 404, never a 403.

import { createHash } from 'node:crypto';

export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const VIEWER_IDENTITY = 'ada iversen <adaiversen@example.invalid>';
export const ZERO_OID = '0'.repeat(40);
export const DEFAULT_BRANCH = 'main';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObj {
  type: ObjType;
  data: Buffer;
}

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: ObjType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([header, data]));
}

export interface CommitView {
  tree: string;
  parents: string[];
  message: string;
  author: string;
  timestamp: number;
}

export function parseCommit(data: Buffer): CommitView {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd === -1 ? text : text.slice(0, headEnd);
  const message = headEnd === -1 ? '' : text.slice(headEnd + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let timestamp = 0;
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) {
      author = line.slice(7);
      const m = / (\d+) [+-]\d{4}$/.exec(author);
      if (m) timestamp = Number(m[1]);
    }
  }
  return { tree, parents, message, author, timestamp };
}

export function buildCommit(fields: { tree: string; parents: readonly string[]; message: string; timestamp: number; author: string }): Buffer {
  const lines: string[] = [`tree ${fields.tree}`];
  for (const parent of fields.parents) lines.push(`parent ${parent}`);
  const stamp = `${fields.timestamp} +0000`;
  lines.push(`author ${fields.author} ${stamp}`);
  lines.push(`committer ${fields.author} ${stamp}`);
  const msg = fields.message.endsWith('\n') ? fields.message : `${fields.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n\n${msg}`, 'utf8');
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space === -1) break;
    const mode = data.subarray(offset, space).toString('utf8');
    const nul = data.indexOf(0x00, space + 1);
    if (nul === -1) break;
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => (treeSortKey(a) < treeSortKey(b) ? -1 : treeSortKey(a) > treeSortKey(b) ? 1 : 0));
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface TagView {
  object: string;
  type: string;
  tag: string;
  message: string;
}

export function parseTag(data: Buffer): TagView {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd === -1 ? text : text.slice(0, headEnd);
  const message = headEnd === -1 ? '' : text.slice(headEnd + 2);
  let object = '';
  let type = '';
  let tag = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
  }
  return { object, type, tag, message };
}

/** Build trees for a flat path -> blob map, inserting every object into `objects`. */
export function treeFromFiles(objects: Map<string, GitObj>, files: ReadonlyMap<string, Buffer>): string {
  interface Node {
    files: Map<string, Buffer>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, data] of files) {
    const segments = path.split('/');
    let node = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
      const name = segments[i]!;
      if (!node.dirs.has(name)) node.dirs.set(name, make());
      node = node.dirs.get(name)!;
    }
    node.files.set(segments[segments.length - 1]!, data);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, data] of node.files) {
      const oid = hashObject('blob', data);
      if (!objects.has(oid)) objects.set(oid, { type: 'blob', data });
      entries.push({ mode: '100644', name, oid });
    }
    for (const [name, child] of node.dirs) {
      entries.push({ mode: '40000', name, oid: build(child) });
    }
    const data = buildTree(entries);
    const oid = hashObject('tree', data);
    if (!objects.has(oid)) objects.set(oid, { type: 'tree', data });
    return oid;
  };
  return build(root);
}

/** Flatten a tree object into path -> blob bytes (recursive). */
export function flattenTree(objects: ReadonlyMap<string, GitObj>, treeOid: string, prefix: string, out: Map<string, Buffer>): void {
  const obj = objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return;
  for (const entry of parseTree(obj.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(objects, entry.oid, path, out);
    else {
      const blob = objects.get(entry.oid);
      if (blob !== undefined) out.set(path, blob.data);
    }
  }
}

/** Commit distances (BFS over parents) from a set of tips; -1 means unreachable.
 *  `maxDepth` caps the search: nodes at distance >= maxDepth are never expanded
 *  (and nodes past it never enter the map). Callers that only need the first
 *  `depth` levels (a shallow fetch) pass it to avoid walking the whole history. */
export function commitDistances(objects: ReadonlyMap<string, GitObj>, tips: Iterable<string>, maxDepth: number = Infinity): Map<string, number> {
  const dist = new Map<string, number>();
  const queue: string[] = [];
  for (const tip of tips) {
    if (tip === ZERO_OID) continue;
    if (!dist.has(tip)) {
      dist.set(tip, 0);
      queue.push(tip);
    }
  }
  for (let i = 0; i < queue.length; i += 1) {
    const oid = queue[i]!;
    const d = dist.get(oid)!;
    const obj = objects.get(oid);
    if (obj === undefined) continue;
    if (obj.type === 'tag') {
      // A tag's target sits at the tag's own distance, so it is always in range.
      const target = parseTag(obj.data).object;
      if (!dist.has(target)) {
        dist.set(target, d);
        queue.push(target);
      }
      continue;
    }
    if (obj.type !== 'commit') continue;
    // Parents sit one level deeper; never expand past the cap.
    if (d + 1 >= maxDepth) continue;
    for (const parent of parseCommit(obj.data).parents) {
      if (!dist.has(parent)) {
        dist.set(parent, d + 1);
        queue.push(parent);
      }
    }
  }
  return dist;
}

/** All objects reachable from tips (commits, their trees/blobs, tag targets). */
export function collectReachable(objects: ReadonlyMap<string, GitObj>, tips: Iterable<string>): Set<string> {
  const seen = new Set<string>();
  const stack: string[] = [];
  for (const tip of tips) {
    if (tip !== ZERO_OID && !seen.has(tip)) {
      seen.add(tip);
      stack.push(tip);
    }
  }
  while (stack.length > 0) {
    const oid = stack.pop()!;
    const obj = objects.get(oid);
    if (obj === undefined) continue;
    if (obj.type === 'commit') {
      const commit = parseCommit(obj.data);
      if (!seen.has(commit.tree)) {
        seen.add(commit.tree);
        stack.push(commit.tree);
      }
      for (const parent of commit.parents) {
        if (!seen.has(parent)) {
          seen.add(parent);
          stack.push(parent);
        }
      }
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) {
        if (!seen.has(entry.oid)) {
          seen.add(entry.oid);
          stack.push(entry.oid);
        }
      }
    } else if (obj.type === 'tag') {
      const target = parseTag(obj.data).object;
      if (!seen.has(target)) {
        seen.add(target);
        stack.push(target);
      }
    }
  }
  return seen;
}

/** A merge base: the common ancestor minimising the sum of distances. */
export function mergeBase(objects: ReadonlyMap<string, GitObj>, a: string, b: string): string | null {
  const da = commitDistances(objects, [a]);
  const db = commitDistances(objects, [b]);
  let best: string | null = null;
  let bestScore = Infinity;
  for (const [oid, d1] of da) {
    const d2 = db.get(oid);
    if (d2 === undefined) continue;
    const score = d1 + d2;
    if (score < bestScore) {
      bestScore = score;
      best = oid;
    }
  }
  return best;
}

/** Is `ancestor` reachable from `tip` (first-parent and merges alike)?
 *  Walks parent links from the tip and stops the moment the ancestor is found,
 *  so the common fast-forward case (the ancestor is the tip's own parent) costs
 *  one commit parse instead of a BFS over the whole history. */
export function isAncestor(objects: ReadonlyMap<string, GitObj>, ancestor: string, tip: string): boolean {
  if (ancestor === ZERO_OID) return true;
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const stack: string[] = [tip];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = objects.get(oid);
    if (obj === undefined) continue;
    if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
      continue;
    }
    if (obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) stack.push(parent);
  }
  return false;
}

// -------------------------------------------------------------------------------------------
// The repository model
// -------------------------------------------------------------------------------------------

export interface IssueLabel {
  name: string;
  color: string;
}

export interface IssueComment {
  id: number;
  body: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: IssueLabel[];
  comments: IssueComment[];
  createdSeq: number;
  /** Bumped on every mutation; caches of the serialized issue key off it. */
  rev: number;
}

/** Mark an issue mutated, invalidating any cached serialization of it. */
export function touchIssue(issue: Issue): void {
  issue.rev += 1;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  mergeBase: string | null;
  createdSeq: number;
}

let globalIdSeq = 100000;

export function nextGlobalId(): number {
  globalIdSeq += 1;
  return globalIdSeq;
}

export class Repo {
  readonly name: string;
  readonly owner: string;
  readonly id: number;
  privateFlag: boolean;
  description: string;
  readonly defaultBranch: string = DEFAULT_BRANCH;
  readonly createdSeq: number;
  readonly objects = new Map<string, GitObj>();
  readonly refs = new Map<string, string>();
  /** Branch name -> flat file snapshot (path -> bytes). */
  readonly files = new Map<string, Map<string, Buffer>>();
  readonly issues: Issue[] = [];
  readonly pulls: Pull[] = [];
  readonly labels = new Map<string, IssueLabel>();
  issueSeq = 0;
  pullSeq = 0;
  commentSeq = 0;

  constructor(id: number, owner: string, name: string, privateFlag: boolean, description: string, createdSeq: number) {
    this.id = id;
    this.owner = owner;
    this.name = name;
    this.privateFlag = privateFlag;
    this.description = description;
    this.createdSeq = createdSeq;
  }

  get key(): string {
    return `${this.owner}/${this.name}`;
  }

  refTip(ref: string): string | undefined {
    return this.refs.get(ref);
  }

  branchTip(branch: string): string | undefined {
    return this.refs.get(`refs/heads/${branch}`);
  }

  /** Point a ref at an oid; branch refs refresh their file snapshot from the commit. */
  setRef(ref: string, oid: string): void {
    this.refs.set(ref, oid);
    if (ref.startsWith('refs/heads/')) {
      const branch = ref.slice('refs/heads/'.length);
      const snapshot = new Map<string, Buffer>();
      const obj = this.objects.get(oid);
      if (obj !== undefined && obj.type === 'commit') {
        flattenTree(this.objects, parseCommit(obj.data).tree, '', snapshot);
      }
      this.files.set(branch, snapshot);
    }
  }

  deleteRef(ref: string): boolean {
    if (!this.refs.has(ref)) return false;
    this.refs.delete(ref);
    if (ref.startsWith('refs/heads/')) this.files.delete(ref.slice('refs/heads/'.length));
    return true;
  }

  branchFiles(branch: string): Map<string, Buffer> {
    let snapshot = this.files.get(branch);
    if (snapshot === undefined) {
      snapshot = new Map<string, Buffer>();
      this.files.set(branch, snapshot);
    }
    return snapshot;
  }

  /** Commit the branch snapshot as a new commit on `branch`; returns its oid. */
  commitBranch(store: Store, branch: string, message: string): string {
    const tip = this.branchTip(branch);
    const tree = treeFromFiles(this.objects, this.branchFiles(branch));
    const parents = tip !== undefined && tip !== ZERO_OID ? [tip] : [];
    const data = buildCommit({ tree, parents, message, timestamp: store.nextTimestamp(), author: VIEWER_IDENTITY });
    const oid = hashObject('commit', data);
    this.objects.set(oid, { type: 'commit', data });
    this.refs.set(`refs/heads/${branch}`, oid);
    return oid;
  }

  /** The first-parent history of a tip, newest first. */
  history(tip: string | undefined): string[] {
    const out: string[] = [];
    let current = tip;
    const seen = new Set<string>();
    while (current !== undefined && current !== ZERO_OID && !seen.has(current)) {
      seen.add(current);
      const obj = this.objects.get(current);
      if (obj === undefined || obj.type !== 'commit') break;
      out.push(current);
      const commit = parseCommit(obj.data);
      current = commit.parents[0];
    }
    return out;
  }

  /** Diff two trees: path -> {status, oldContent, newContent}. */
  diffTrees(baseTree: string | undefined, headTree: string): Array<{ path: string; status: 'added' | 'modified' | 'removed'; old: Buffer | null; current: Buffer }> {
    const baseFiles = new Map<string, Buffer>();
    if (baseTree !== undefined && baseTree !== ZERO_OID) flattenTree(this.objects, baseTree, '', baseFiles);
    const headFiles = new Map<string, Buffer>();
    flattenTree(this.objects, headTree, '', headFiles);
    const out: Array<{ path: string; status: 'added' | 'modified' | 'removed'; old: Buffer | null; current: Buffer }> = [];
    for (const [path, data] of headFiles) {
      const old = baseFiles.get(path);
      if (old === undefined) out.push({ path, status: 'added', old: null, current: data });
      else if (!old.equals(data)) out.push({ path, status: 'modified', old, current: data });
    }
    for (const path of baseFiles.keys()) {
      if (!headFiles.has(path)) out.push({ path, status: 'removed', old: baseFiles.get(path) ?? null, current: Buffer.alloc(0) });
    }
    out.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
    return out;
  }
}

export class Store {
  readonly repos = new Map<string, Repo>();
  private repoSeq = 5000;
  private seq = 0;
  private clock = 1735689600;
  /** Global ids (Relay): kind + key -> opaque id, and back. */
  private readonly gidMap = new Map<string, string>();
  private readonly gidReverse = new Map<string, { kind: string; key: string }>();

  nextSeq(): number {
    this.seq += 1;
    return this.seq;
  }

  nextTimestamp(): number {
    this.clock += 1;
    return this.clock;
  }

  isoNow(): string {
    return new Date(this.nextTimestamp() * 1000).toISOString();
  }

  globalId(kind: string, key: string): string {
    const mapKey = `${kind}:${key}`;
    let id = this.gidMap.get(mapKey);
    if (id === undefined) {
      id = `${kind[0]!.toUpperCase()}_` + Buffer.from(mapKey, 'utf8').toString('base64url');
      this.gidMap.set(mapKey, id);
      this.gidReverse.set(id, { kind, key });
    }
    return id;
  }

  decodeGlobalId(id: string): { kind: string; key: string } | null {
    return this.gidReverse.get(id) ?? null;
  }

  createRepo(name: string, privateFlag: boolean, description: string): Repo | null {
    const key = `${VIEWER_LOGIN}/${name}`;
    if (this.repos.has(key)) return null;
    this.repoSeq += 1;
    const repo = new Repo(this.repoSeq, VIEWER_LOGIN, name, privateFlag, description, this.nextSeq());
    this.repos.set(key, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | null {
    if (owner !== VIEWER_LOGIN) return null;
    return this.repos.get(`${owner}/${name}`) ?? null;
  }

  listRepos(): Repo[] {
    return [...this.repos.values()].sort((a, b) => a.createdSeq - b.createdSeq);
  }
}

/** A repo as the REST v3 API returns it. */
export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: `R_` + Buffer.from(`repo:${repo.owner}/${repo.name}`, 'utf8').toString('base64url'),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    private: repo.privateFlag,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: new Date(1735689600 * 1000).toISOString(),
    updated_at: new Date(1735689600 * 1000).toISOString(),
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    fork: false,
    archived: false,
    disabled: false,
    visibility: repo.privateFlag ? 'private' : 'public',
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
  };
}

export function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: `I_` + Buffer.from(`issue:${repo.owner}/${repo.name}:${issue.number}`, 'utf8').toString('base64url'),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((label) => ({ name: label.name, color: label.color })),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    comments: issue.comments.length,
    created_at: new Date((1735689600 + issue.createdSeq) * 1000).toISOString(),
    updated_at: new Date((1735689600 + issue.createdSeq) * 1000).toISOString(),
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

export function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: `P_` + Buffer.from(`pull:${repo.owner}/${repo.name}:${pull.number}`, 'utf8').toString('base64url'),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.head, sha: pull.headSha, label: `${repo.owner}:${pull.head}` },
    base: { ref: pull.base, sha: pull.baseSha, label: `${repo.owner}:${pull.base}` },
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: new Date((1735689600 + pull.createdSeq) * 1000).toISOString(),
    updated_at: new Date((1735689600 + pull.createdSeq) * 1000).toISOString(),
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

/** Line counts for a pull file entry: added/deleted lines, at least 1 addition when changed. */
export function lineCounts(oldBuf: Buffer | null, newBuf: Buffer): { additions: number; deletions: number } {
  if (oldBuf === null) {
    const lines = newBuf.toString('utf8').split('\n').filter((line) => line.length > 0).length;
    return { additions: Math.max(1, lines), deletions: 0 };
  }
  const oldLines = new Set(oldBuf.toString('utf8').split('\n'));
  const newLines = newBuf.toString('utf8').split('\n');
  let additions = 0;
  for (const line of newLines) if (!oldLines.has(line)) additions += 1;
  const newSet = new Set(newLines);
  let deletions = 0;
  for (const line of oldBuf.toString('utf8').split('\n')) if (!newSet.has(line)) deletions += 1;
  if (additions === 0 && deletions === 0) additions = 1;
  return { additions, deletions };
}
