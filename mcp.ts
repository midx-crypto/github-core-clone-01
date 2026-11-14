// MCP over Streamable HTTP (JSON-RPC 2.0). Two eras are served at once:
//   - modern 2026-07-28: server/discover answers supportedVersions, no handshake;
//   - legacy 2025-11-25 and earlier: initialize + notifications/initialized.
// Every tools/call runs against the same store REST and GraphQL use, so an
// issue created over MCP reads back over /api/v3 with the same id.

import {
  VIEWER_LOGIN,
  issueJson,
  pullJson,
  repoJson,
  type Repo,
  type Store,
} from './store.ts';
import { squashMerge } from './graphql.ts';
import { mergeBase } from './store.ts';

export interface ToolDef {
  name: string;
  description: string;
  properties: Record<string, { type: string; description: string }>;
  required: string[];
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    properties: {
      name: { type: 'string', description: 'The repository name.' },
      description: { type: 'string', description: 'A short description of the repository.' },
      private: { type: 'boolean', description: 'Whether the repository is private.' },
      owner: { type: 'string', description: 'The owner login (defaults to the authenticated user).' },
    },
    required: ['name'],
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The issue title.' },
      body: { type: 'string', description: 'The issue body.' },
    },
    required: ['owner', 'repo', 'title'],
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', description: 'Filter by state: open, closed, or all.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The pull request title.' },
      head: { type: 'string', description: 'The head branch name.' },
      base: { type: 'string', description: 'The base branch name.' },
      body: { type: 'string', description: 'The pull request body.' },
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      pull_number: { type: 'integer', description: 'The pull request number.' },
    },
    required: ['owner', 'repo', 'pull_number'],
  },
  {
    name: 'get_issue',
    description: 'Get one issue by number.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      issue_number: { type: 'integer', description: 'The issue number.' },
    },
    required: ['owner', 'repo', 'issue_number'],
  },
  {
    name: 'list_pull_requests',
    description: 'List pull requests in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', description: 'Filter by state: open, closed, or all.' },
    },
    required: ['owner', 'repo'],
  },
];

function toolsListResult(): Record<string, unknown> {
  return {
    tools: TOOLS.map((tool) => ({
      name: tool.name,
      description: tool.description,
      inputSchema: { type: 'object', properties: tool.properties, required: tool.required },
    })),
  };
}

function textResult(value: unknown): Record<string, unknown> {
  return { content: [{ type: 'text', text: JSON.stringify(value) }], structuredContent: value, isError: false };
}

function errorResult(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function findTool(name: string): ToolDef | null {
  const normalized = name.toLowerCase().replace(/[^a-z0-9]/g, '');
  for (const tool of TOOLS) {
    const wanted = tool.name.replace(/[^a-z0-9]/g, '');
    if (normalized === wanted || (normalized.length > wanted.length && normalized.endsWith(wanted))) return tool;
  }
  return null;
}

function getRepoOrError(store: Store, args: Record<string, unknown>): Repo | { error: string } {
  const owner = typeof args.owner === 'string' ? args.owner : VIEWER_LOGIN;
  const repoName = typeof args.repo === 'string' ? args.repo : '';
  const repo = store.getRepo(owner, repoName);
  if (repo === null) return { error: `repository ${owner}/${repoName} not found` };
  return repo;
}

function callTool(store: Store, name: string, args: Record<string, unknown>): { result: Record<string, unknown>; rpcError?: { code: number; message: string } } {
  const tool = findTool(name);
  if (tool === null) return { result: {}, rpcError: { code: -32602, message: `unknown tool: ${name}` } };
  for (const required of tool.required) {
    if (args[required] === undefined || args[required] === null || args[required] === '') {
      return { result: errorResult(`missing required argument: ${required}`) };
    }
  }
  if (tool.name === 'create_repo') {
    const repoName = String(args.name);
    const existing = store.getRepo(VIEWER_LOGIN, repoName);
    if (existing !== null) return { result: textResult(repoJson(existing)) };
    const repo = store.createRepo(repoName, args.private === true, typeof args.description === 'string' ? args.description : '');
    if (repo === null) return { result: errorResult(`repository ${repoName} already exists`) };
    return { result: textResult(repoJson(repo)) };
  }
  if (tool.name === 'get_repo') {
    const repo = getRepoOrError(store, args);
    if ('error' in repo) return { result: errorResult(repo.error) };
    return { result: textResult(repoJson(repo)) };
  }
  if (tool.name === 'create_issue') {
    const repo = getRepoOrError(store, args);
    if ('error' in repo) return { result: errorResult(repo.error) };
    repo.issueSeq += 1;
    const issue = {
      id: 100000 + repo.id * 1000 + repo.issueSeq,
      number: repo.issueSeq,
      title: String(args.title),
      body: typeof args.body === 'string' ? args.body : '',
      state: 'open' as const,
      labels: [] as Array<{ name: string; color: string }>,
      comments: [] as Array<{ id: number; body: string }>,
      createdSeq: store.nextSeq(),
      rev: 0,
    };
    repo.issues.push(issue);
    return { result: textResult(issueJson(repo, issue)) };
  }
  if (tool.name === 'list_issues') {
    const repo = getRepoOrError(store, args);
    if ('error' in repo) return { result: errorResult(repo.error) };
    const state = typeof args.state === 'string' ? args.state.toLowerCase() : 'open';
    const issues = repo.issues.filter((issue) => state === 'all' || issue.state === state);
    return { result: textResult(issues.map((issue) => issueJson(repo, issue))) };
  }
  if (tool.name === 'get_issue') {
    const repo = getRepoOrError(store, args);
    if ('error' in repo) return { result: errorResult(repo.error) };
    const number = Number(args.issue_number);
    const issue = repo.issues.find((entry) => entry.number === number);
    if (issue === undefined) return { result: errorResult(`issue #${number} not found`) };
    return { result: textResult(issueJson(repo, issue)) };
  }
  if (tool.name === 'create_pull_request') {
    const repo = getRepoOrError(store, args);
    if ('error' in repo) return { result: errorResult(repo.error) };
    const head = String(args.head);
    const base = String(args.base);
    const headSha = repo.branchTip(head) ?? '';
    const baseSha = repo.branchTip(base) ?? '';
    if (headSha === '') return { result: errorResult(`head branch ${head} not found`) };
    repo.pullSeq += 1;
    const pull = {
      id: 200000 + repo.id * 1000 + repo.pullSeq,
      number: repo.pullSeq,
      title: String(args.title),
      body: typeof args.body === 'string' ? args.body : '',
      state: 'open' as const,
      merged: false,
      mergedAt: null as string | null,
      mergeCommitSha: null as string | null,
      head,
      base,
      headSha,
      baseSha,
      mergeBase: headSha !== '' && baseSha !== '' ? mergeBase(repo.objects, headSha, baseSha) : null,
      createdSeq: store.nextSeq(),
    };
    repo.pulls.push(pull);
    return { result: textResult(pullJson(repo, pull)) };
  }
  if (tool.name === 'merge_pull_request') {
    const repo = getRepoOrError(store, args);
    if ('error' in repo) return { result: errorResult(repo.error) };
    const number = Number(args.pull_number);
    const pull = repo.pulls.find((entry) => entry.number === number);
    if (pull === undefined) return { result: errorResult(`pull request #${number} not found`) };
    const merged = squashMerge(store, repo, pull);
    if (merged === null) return { result: errorResult(`pull request #${number} has merge conflicts`) };
    return { result: textResult(pullJson(repo, pull)) };
  }
  if (tool.name === 'list_pull_requests') {
    const repo = getRepoOrError(store, args);
    if ('error' in repo) return { result: errorResult(repo.error) };
    const state = typeof args.state === 'string' ? args.state.toLowerCase() : 'open';
    const pulls = repo.pulls.filter((pull) => state === 'all' || pull.state === state);
    return { result: textResult(pulls.map((pull) => pullJson(repo, pull))) };
  }
  return { result: {}, rpcError: { code: -32602, message: `unknown tool: ${name}` } };
}

export interface McpResponse {
  status: number;
  body: Buffer;
  headers: Record<string, string>;
}

/** Handle one POST /mcp body. `authorized` is false when no valid bearer was sent. */
export function handleMcp(store: Store, rawBody: Buffer, authorized: boolean): McpResponse {
  const jsonHeaders = { 'content-type': 'application/json; charset=utf-8' };
  if (!authorized) {
    return { status: 401, body: Buffer.from(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32000, message: 'Unauthorized' } }) + '\n', 'utf8'), headers: jsonHeaders };
  }
  const text = rawBody.toString('utf8');
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    const idMatch = /"id"\s*:\s*("[^"]*"|\d+)/.exec(text);
    let id: unknown = null;
    if (idMatch !== null) {
      const raw = idMatch[1]!;
      id = raw.startsWith('"') ? raw.slice(1, -1) : Number(raw);
    }
    return {
      status: 400,
      body: Buffer.from(JSON.stringify({ jsonrpc: '2.0', id, error: { code: -32700, message: 'Parse error' } }) + '\n', 'utf8'),
      headers: jsonHeaders,
    };
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { status: 400, body: Buffer.from(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } }) + '\n', 'utf8'), headers: jsonHeaders };
  }
  const request = parsed as Record<string, unknown>;
  // Every reply below echoes this request's own id. Two concurrent tools/call
  // requests each keep their id; nothing here renumbers or shares one.
  const id = (request.id ?? null) as string | number | null;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = (request.params ?? {}) as Record<string, unknown>;
  // Notifications carry no id: acknowledge with 202 and no body.
  if (request.id === undefined) {
    return { status: 202, body: Buffer.alloc(0), headers: {} };
  }
  const ok = (result: unknown, extraHeaders: Record<string, string> = {}): McpResponse => ({
    status: 200,
    body: Buffer.from(JSON.stringify({ jsonrpc: '2.0', id, result }) + '\n', 'utf8'),
    headers: { ...jsonHeaders, ...extraHeaders },
  });
  const err = (code: number, message: string): McpResponse => ({
    status: 200,
    body: Buffer.from(JSON.stringify({ jsonrpc: '2.0', id, error: { code, message } }) + '\n', 'utf8'),
    headers: jsonHeaders,
  });
  if (method === 'server/discover') {
    return ok({
      resultType: 'discover',
      supportedVersions: ['2026-07-28', '2025-11-25', '2025-06-18'],
      protocolVersion: '2026-07-28',
      serverInfo: { name: 'forge', version: '1.0.0' },
      capabilities: { tools: {} },
    });
  }
  if (method === 'initialize') {
    const offered = (params.protocolVersion as string | undefined) ?? '2025-11-25';
    const known = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
    const negotiated = known.includes(offered) ? offered : '2025-11-25';
    return ok(
      {
        protocolVersion: negotiated,
        serverInfo: { name: 'forge', version: '1.0.0' },
        capabilities: { tools: {} },
      },
      { 'mcp-session-id': 'forge-session-1' },
    );
  }
  if (method === 'ping') return ok({});
  if (method === 'tools/list') return ok(toolsListResult());
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const outcome = callTool(store, name, args);
    if (outcome.rpcError !== undefined) return err(outcome.rpcError.code, outcome.rpcError.message);
    return ok(outcome.result);
  }
  return err(-32601, `Method not found: ${method}`);
}
