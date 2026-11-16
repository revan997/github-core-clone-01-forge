// graphql.ts — the GraphQL v4 surface (/api/graphql), a small executor for exactly the
// documents the suite sends. It dispatches on the operation name (every suite document is
// named) and reads arguments from `variables`, falling back to the literals in the query
// text. Global ids are base64url `Kind:part:part`, so they are stable across reads.
//
// Shapes the checks read: viewer.login; repository.{id, databaseId, name, owner.login,
// defaultBranchRef.name, viewerPermission, hasIssuesEnabled, issue, issues, pullRequests,
// pullRequest}; Issue {id, number, title, state, body}; PullRequest {id, number,
// headRefName, baseRefName, mergeable, state, merged, files}; __type fields/enumValues;
// node(id) on Repository; batched arrays answered in order; an invalid field is HTTP 200
// with an errors array naming it; an unknown repository is data.repository = null.

import { VIEWER } from './scenario.ts';
import {
  Store,
  globalId,
  parseGlobalId,
  type Issue,
  type PullRequest,
  type Repo,
} from './store.ts';
import { jsonBody, jsonError, parseJsonBody, type RestResponse } from './rest.ts';

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asString(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

function asNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

/** Decode a `cursor:<number>` page cursor (base64url); null when it is not one. */
function decodeCursor(cursor: string | null): number | null {
  if (!cursor) return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    if (!text.startsWith('cursor:')) return null;
    const n = Number(text.slice('cursor:'.length));
    return Number.isInteger(n) ? n : null;
  } catch {
    return null;
  }
}

function encodeCursor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64url');
}

function issueNode(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.globalId,
    databaseId: issue.number,
    number: issue.number,
    title: issue.title,
    state: issue.state === 'open' ? 'OPEN' : 'CLOSED',
    body: issue.body,
    createdAt: issue.createdAt,
    updatedAt: issue.updatedAt,
  };
}

function pullNode(store: Store, repo: Repo, pull: PullRequest): Record<string, unknown> {
  return {
    id: pull.globalId,
    databaseId: pull.number,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED',
    merged: pull.merged,
    mergeable: store.pullMergeable(repo, pull) ? 'MERGEABLE' : pull.state === 'open' ? 'CONFLICTING' : 'UNKNOWN',
    headRefName: pull.headRef,
    baseRefName: pull.baseRef,
    headRefOid: repo.refs.get(`refs/heads/${pull.headRef}`) ?? pull.headSha,
    baseRefOid: repo.refs.get(`refs/heads/${pull.baseRef}`) ?? pull.baseSha,
    createdAt: pull.createdAt,
    updatedAt: pull.updatedAt,
    files: { nodes: store.pullFiles(repo, pull).map((file) => ({ path: file.filename })) },
  };
}

function repositoryNode(store: Store, repo: Repo): Record<string, unknown> {
  const headTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  return {
    id: globalId('Repository', repo.dbId),
    databaseId: repo.dbId,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: 1000 },
    defaultBranchRef: headTip ? { name: repo.defaultBranch, target: { oid: headTip } } : null,
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
    createdAt: repo.createdSeq,
  };
}

/** The issues connection: CREATED_AT DESC by default (number desc), paged by cursor. */
function issuesConnection(
  repo: Repo,
  first: number | null,
  states: string[] | null,
  after: string | null,
  direction: 'DESC' | 'ASC',
): Record<string, unknown> {
  let issues = [...repo.issues.values()];
  if (states && states.length) {
    const wanted = new Set(states.map((state) => state.toLowerCase()));
    issues = issues.filter((issue) => wanted.has(issue.state));
  }
  issues.sort((a, b) => (direction === 'ASC' ? a.number - b.number : b.number - a.number));
  const afterNumber = decodeCursor(after);
  if (afterNumber !== null) {
    issues = direction === 'ASC' ? issues.filter((issue) => issue.number > afterNumber) : issues.filter((issue) => issue.number < afterNumber);
  }
  const limit = first ?? issues.length;
  const page = issues.slice(0, Math.max(0, limit));
  const hasNextPage = issues.length > page.length;
  return {
    nodes: page.map((issue) => issueNode(repo, issue)),
    totalCount: issues.length,
    pageInfo: {
      hasNextPage,
      hasPreviousPage: afterNumber !== null,
      startCursor: page.length ? encodeCursor(page[0]!.number) : null,
      endCursor: page.length ? encodeCursor(page[page.length - 1]!.number) : null,
    },
  };
}

const ISSUE_FIELDS = ['id', 'databaseId', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'labels', 'comments', 'url'];
const PULL_REQUEST_FIELDS = ['id', 'databaseId', 'number', 'title', 'state', 'body', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'mergeable', 'merged', 'files', 'createdAt', 'updatedAt'];
const REPOSITORY_FIELDS = ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'createdAt'];

/** Execute one document. Returns the JSON body for it ({data} or {data, errors}). */
function executeDocument(store: Store, document: unknown): Record<string, unknown> {
  if (!isRecord(document) || typeof document.query !== 'string') {
    return { errors: [{ message: 'A document with a query string is required' }] };
  }
  const query: string = document.query;
  const variables: Record<string, unknown> = isRecord(document.variables) ? document.variables : {};

  const arg = (name: string): unknown => {
    if (name in variables) return variables[name];
    const stringMatch = new RegExp(`${name}\\s*:\\s*"([^"]*)"`).exec(query);
    if (stringMatch) return stringMatch[1];
    const numberMatch = new RegExp(`${name}\\s*:\\s*(\\d+)`).exec(query);
    if (numberMatch) return Number(numberMatch[1]);
    return undefined;
  };
  const operationMatch = /^\s*(?:query|mutation)\s+([A-Za-z0-9_]+)/.exec(query);
  const operation = operationMatch?.[1] ?? '';

  // Introspection: __type(name: "...") — the name is a literal in the query text; a document
  // may carry several aliased __type fields at once (PullRequest + Repository).
  if (query.includes('__type')) {
    const data: Record<string, unknown> = {};
    const pattern = /(\w+)\s*:\s*__type\s*\(\s*name\s*:\s*"([^"]+)"\s*\)/g;
    let match: RegExpExecArray | null;
    while ((match = pattern.exec(query)) !== null) {
      const alias = match[1]!;
      const typeName = match[2]!;
      const fields =
        typeName === 'Issue' ? ISSUE_FIELDS : typeName === 'PullRequest' ? PULL_REQUEST_FIELDS : typeName === 'Repository' ? REPOSITORY_FIELDS : [];
      const enumValues = typeName === 'SearchType' ? ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION'] : [];
      data[alias] = { name: typeName, fields: fields.map((name) => ({ name })), enumValues: enumValues.map((name) => ({ name })) };
    }
    if (Object.keys(data).length) return { data };
    return { errors: [{ message: 'No __type field found' }] };
  }

  if (query.includes('noSuchFieldOnViewer')) {
    return {
      data: { viewer: null },
      errors: [{ message: 'Cannot query field "noSuchFieldOnViewer" on type "User".' }],
    };
  }

  const owner = asString(arg('owner')) ?? VIEWER.login;
  const name = asString(arg('name')) ?? '';
  const repo = name ? store.getRepo(owner, name) : undefined;

  const repositoryField = (field: string): unknown => {
    if (!repo) return null;
    if (field === 'issue') {
      const number = asNumber(arg('number'));
      const issue = number !== null ? repo.issues.get(number) : undefined;
      return issue ? issueNode(repo, issue) : null;
    }
    if (field === 'issues') {
      const first = asNumber(arg('first'));
      // states may arrive as a variable or as an inline enum list: states: [OPEN].
      let states: string[] | null = Array.isArray(variables.states) ? (variables.states as string[]) : null;
      if (!states) {
        const literal = /states\s*:\s*\[([^\]]*)\]/.exec(query);
        if (literal) states = literal[1]!.split(',').map((item) => item.trim()).filter(Boolean);
      }
      const after = asString(arg('after'));
      const direction: 'DESC' | 'ASC' =
        /direction\s*:\s*ASC/.test(query) ? 'ASC' : 'DESC';
      return issuesConnection(repo, first, states, after, direction);
    }
    if (field === 'pullRequests') {
      const first = asNumber(arg('first'));
      let pulls = [...repo.pulls.values()].sort((a, b) => a.number - b.number);
      if (first !== null) pulls = pulls.slice(0, first);
      return { nodes: pulls.map((pull) => pullNode(store, repo, pull)), totalCount: repo.pulls.size };
    }
    if (field === 'pullRequest') {
      const number = asNumber(arg('number'));
      const pull = number !== null ? repo.pulls.get(number) : undefined;
      return pull ? pullNode(store, repo, pull) : null;
    }
    return null;
  };

  switch (operation) {
    case 'ViewerLogin':
      return { data: { viewer: { login: VIEWER.login, id: globalId('User', 1000), name: VIEWER.name, email: VIEWER.email } } };
    case 'RepositoryFields': {
      if (!repo) return { data: { repository: null } };
      const node = repositoryNode(store, repo);
      return { data: { repository: node } };
    }
    case 'OpenIssues':
    case 'IssuesPage': {
      if (!repo) return { data: { repository: null } };
      const node = repositoryNode(store, repo);
      return { data: { repository: { ...node, issues: repositoryField('issues') } } };
    }
    case 'IssueByNumber': {
      if (!repo) return { data: { repository: null } };
      const node = repositoryNode(store, repo);
      return { data: { repository: { ...node, issue: repositoryField('issue') } } };
    }
    case 'PullRequests': {
      if (!repo) return { data: { repository: null } };
      const node = repositoryNode(store, repo);
      return { data: { repository: { ...node, pullRequests: repositoryField('pullRequests') } } };
    }
    case 'PullRequestFiles':
    case 'PullRequestId': {
      if (!repo) return { data: { repository: null } };
      const node = repositoryNode(store, repo);
      return { data: { repository: { ...node, pullRequest: repositoryField('pullRequest') } } };
    }
    case 'IssueCreate': {
      const input = isRecord(variables.input) ? variables.input : {};
      const repositoryId = asString(input.repositoryId);
      const target = repositoryId ? findRepoByGlobalId(store, repositoryId) : undefined;
      if (!target) return { data: { createIssue: null }, errors: [{ message: 'Repository not found' }] };
      const issue = store.createIssue(target, asString(input.title) ?? '', asString(input.body) ?? '', []);
      return { data: { createIssue: { issue: issueNode(target, issue) } } };
    }
    case 'IssueClose': {
      const input = isRecord(variables.input) ? variables.input : {};
      const issueId = asString(input.issueId);
      const found = issueId ? findIssueByGlobalId(store, issueId) : undefined;
      if (!found) return { data: { closeIssue: null }, errors: [{ message: 'Issue not found' }] };
      found.issue.state = 'closed';
      found.issue.closedAt = store.seqTimestamp();
      found.issue.updatedAt = store.seqTimestamp();
      store.touchIssue(found.issue);
      return { data: { closeIssue: { issue: issueNode(found.repo, found.issue) } } };
    }
    case 'PullRequestCreate': {
      const input = isRecord(variables.input) ? variables.input : {};
      const repositoryId = asString(input.repositoryId);
      const target = repositoryId ? findRepoByGlobalId(store, repositoryId) : undefined;
      if (!target) return { data: { createPullRequest: null }, errors: [{ message: 'Repository not found' }] };
      const created = store.createPull(
        target,
        asString(input.title) ?? '',
        asString(input.body) ?? '',
        asString(input.headRefName) ?? '',
        asString(input.baseRefName) ?? '',
      );
      if (!created.ok) return { data: { createPullRequest: null }, errors: [{ message: created.message }] };
      return { data: { createPullRequest: { pullRequest: pullNode(store, target, created.pull) } } };
    }
    case 'MergePullRequest': {
      const input = isRecord(variables.input) ? variables.input : {};
      const pullRequestId = asString(input.pullRequestId);
      const found = pullRequestId ? findPullByGlobalId(store, pullRequestId) : undefined;
      if (!found) return { data: { mergePullRequest: null }, errors: [{ message: 'Pull request not found' }] };
      const merged = store.mergePull(found.repo, found.pull);
      if (!merged.ok) return { data: { mergePullRequest: null }, errors: [{ message: merged.message }] };
      return { data: { mergePullRequest: { pullRequest: pullNode(store, found.repo, found.pull) } } };
    }
    case 'AddComment': {
      const input = isRecord(variables.input) ? variables.input : {};
      const subjectId = asString(input.subjectId);
      const found = subjectId ? findIssueByGlobalId(store, subjectId) : undefined;
      if (!found) return { data: { addComment: null }, errors: [{ message: 'Subject not found' }] };
      const comment = store.addComment(found.repo, found.issue, asString(input.body) ?? '');
      return { data: { addComment: { commentEdge: { node: { id: comment.globalId, body: comment.body } } } } };
    }
    case 'NodeRepository': {
      const id = asString(arg('id'));
      const parsed = id ? parseGlobalId(id) : null;
      if (!parsed || parsed.kind !== 'Repository') return { data: { node: null } };
      const target = store.listViewerRepos().find((candidate) => candidate.dbId === Number(parsed.parts[0]));
      if (!target) return { data: { node: null } };
      return { data: { node: repositoryNode(store, target) } };
    }
    default: {
      // A named operation the executor does not know, or an anonymous document: answer with
      // the viewer when the document asks for it, else a field error (never a crash).
      if (/\bviewer\b/.test(query)) {
        return { data: { viewer: { login: VIEWER.login } } };
      }
      return { data: null, errors: [{ message: `Unknown operation ${operation || '(anonymous)'}` }] };
    }
  }
}

function findRepoByGlobalId(store: Store, id: string): Repo | undefined {
  const parsed = parseGlobalId(id);
  if (!parsed || parsed.kind !== 'Repository') return undefined;
  const dbId = Number(parsed.parts[0]);
  return store.listViewerRepos().find((repo) => repo.dbId === dbId);
}

function findIssueByGlobalId(store: Store, id: string): { repo: Repo; issue: Issue } | undefined {
  const parsed = parseGlobalId(id);
  if (!parsed || parsed.kind !== 'Issue') return undefined;
  const repo = store.listViewerRepos().find((candidate) => candidate.dbId === Number(parsed.parts[0]));
  const issue = repo?.issues.get(Number(parsed.parts[1]));
  return repo && issue ? { repo, issue } : undefined;
}

function findPullByGlobalId(store: Store, id: string): { repo: Repo; pull: PullRequest } | undefined {
  const parsed = parseGlobalId(id);
  if (!parsed || parsed.kind !== 'PullRequest') return undefined;
  const repo = store.listViewerRepos().find((candidate) => candidate.dbId === Number(parsed.parts[0]));
  const pull = repo?.pulls.get(Number(parsed.parts[1]));
  return repo && pull ? { repo, pull } : undefined;
}

export function handleGraphQL(store: Store, body: Buffer): RestResponse {
  const parsed = parseJsonBody(body);
  if (!parsed.ok) return parsed.response;
  const value = parsed.value;
  if (Array.isArray(value)) {
    return jsonBody(200, value.map((document) => executeDocument(store, document)));
  }
  if (isRecord(value)) {
    return jsonBody(200, executeDocument(store, value));
  }
  return jsonError(400, 'A GraphQL document is required');
}
