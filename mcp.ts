// mcp.ts — the Model Context Protocol endpoint (/mcp), modern era (2026-07-28).
//
// The suite's client probes `server/discover` first: a result makes the era modern (no
// handshake, no session); a 404 or -32601 falls back to the legacy initialize handshake.
// This server answers discover, so it is modern — and it still answers `initialize` for
// any client that asks. Transport: plain JSON over one POST per request (the client reads
// a JSON-RPC body, or an SSE stream; JSON is enough).
//
// Tool contract (from checks-mcp.ts): every tool has a non-empty description and an
// inputSchema of type object; the six required tools are create_repo, get_repo,
// create_issue, list_issues, create_pull_request, merge_pull_request. Tool arguments use
// the documented property names, so the suite's mapper finds them by exact name. A tool
// result carries both text and structuredContent, and a tool failure is isError:true —
// never an HTTP 500. An unknown tool is JSON-RPC -32602; a body that is not JSON is
// HTTP 400 with -32700.

import { VIEWER } from './scenario.ts';
import { Store, type Repo } from './store.ts';

const SERVER_NAME = 'makefun-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18'];

interface McpToolDef {
  readonly name: string;
  readonly description: string;
  readonly inputSchema: { type: 'object'; properties: Record<string, { type: string; description: string }>; required: string[] };
}

const TOOLS: readonly McpToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state: open, closed, or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'number', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function toolResult(data: unknown, isError = false): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: JSON.stringify(data) }],
    structuredContent: data,
    isError,
  };
}

function toolError(message: string): Record<string, unknown> {
  return toolResult({ error: message }, true);
}

function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.dbId,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
  };
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const tool = TOOLS.find((candidate) => candidate.name === name);
  if (!tool) return toolError(`unknown tool ${name}`);
  for (const required of tool.inputSchema.required) {
    if (args[required] === undefined || args[required] === null || args[required] === '') {
      return toolError(`missing required argument: ${required}`);
    }
  }
  const owner = typeof args.owner === 'string' ? args.owner : VIEWER.login;
  const repoName = typeof args.repo === 'string' ? args.repo : '';
  switch (name) {
    case 'create_repo': {
      const repo = store.createRepo(String(args.name), args.private === true, typeof args.description === 'string' ? args.description : '');
      if (!repo) return toolError(`Repository ${args.name} already exists`);
      return toolResult(repoJson(repo));
    }
    case 'get_repo': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found`);
      return toolResult(repoJson(repo));
    }
    case 'create_issue': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found`);
      const issue = store.createIssue(repo, String(args.title), typeof args.body === 'string' ? args.body : '', []);
      return toolResult({
        id: issue.globalId,
        number: issue.number,
        title: issue.title,
        body: issue.body,
        state: issue.state,
        url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
      });
    }
    case 'list_issues': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found`);
      const state = typeof args.state === 'string' ? args.state : 'open';
      let issues = [...repo.issues.values()];
      if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
      issues.sort((a, b) => a.number - b.number);
      return toolResult({
        issues: issues.map((issue) => ({ id: issue.globalId, number: issue.number, title: issue.title, body: issue.body, state: issue.state })),
      });
    }
    case 'create_pull_request': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found`);
      const created = store.createPull(
        repo,
        String(args.title),
        '',
        String(args.head).split(':').pop() ?? '',
        String(args.base).split(':').pop() ?? '',
      );
      if (!created.ok) return toolError(created.message);
      const pull = created.pull;
      return toolResult({
        id: pull.globalId,
        number: pull.number,
        pull_number: pull.number,
        pull_request_number: pull.number,
        title: pull.title,
        state: pull.state,
        head: pull.headRef,
        base: pull.baseRef,
        headRefName: pull.headRef,
        baseRefName: pull.baseRef,
      });
    }
    case 'merge_pull_request': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(`Repository ${owner}/${repoName} not found`);
      const pull = repo.pulls.get(Number(args.pull_number));
      if (!pull) return toolError(`Pull request ${args.pull_number} not found`);
      const merged = store.mergePull(repo, pull);
      if (!merged.ok) return toolError(merged.message);
      return toolResult({ number: pull.number, pull_number: pull.number, merged: true, state: 'closed', sha: merged.sha });
    }
    default:
      return toolError(`unknown tool ${name}`);
  }
}

export interface McpResponse {
  readonly status: number;
  readonly body: Buffer;
}

function jsonRpcResponse(status: number, value: unknown): McpResponse {
  return { status, body: Buffer.from(JSON.stringify(value), 'utf8') };
}

export function handleMcp(store: Store, body: Buffer): McpResponse {
  let request: unknown;
  try {
    request = JSON.parse(body.toString('utf8'));
  } catch {
    return jsonRpcResponse(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
  }
  if (!isRecord(request) || typeof request.method !== 'string') {
    return jsonRpcResponse(400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
  }
  const id = 'id' in request ? request.id : null;
  const method = request.method;
  const params = isRecord(request.params) ? request.params : {};
  // A notification (no id) gets no answer.
  const isNotification = !('id' in request) || request.id === null || request.id === undefined;
  const answer = (result: unknown): McpResponse => jsonRpcResponse(200, { jsonrpc: '2.0', id, result });
  const fail = (code: number, message: string): McpResponse =>
    jsonRpcResponse(200, { jsonrpc: '2.0', id, error: { code, message } });

  switch (method) {
    case 'server/discover':
      return answer({
        resultType: 'discover',
        supportedVersions: [...SUPPORTED_VERSIONS],
        capabilities: { tools: { listChanged: false } },
        instructions: 'A GitHub-compatible git forge: create repositories, issues and pull requests over MCP tools.',
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
        _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
      });
    case 'initialize': {
      const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const negotiated = SUPPORTED_VERSIONS.includes(requested) ? requested : '2025-11-25';
      return answer({
        protocolVersion: negotiated,
        capabilities: { tools: { listChanged: false } },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      });
    }
    case 'notifications/initialized':
      return isNotification ? { status: 202, body: Buffer.alloc(0) } : answer({});
    case 'ping':
      return answer({});
    case 'tools/list':
      return answer({ tools: TOOLS });
    case 'tools/call': {
      const toolName = typeof params.name === 'string' ? params.name : '';
      const args = isRecord(params.arguments) ? params.arguments : {};
      if (!toolName) return fail(-32602, 'tools/call needs a tool name');
      if (!TOOLS.some((tool) => tool.name === toolName)) return fail(-32602, `unknown tool ${toolName}`);
      return answer(callTool(store, toolName, args));
    }
    default:
      return fail(-32601, `Method not found: ${method}`);
  }
}
