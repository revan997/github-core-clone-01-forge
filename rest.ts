// rest.ts — the GitHub REST v3 surface (/api/v3/...).
//
// Shapes mirror GitHub's: repo, issue, pull, comment, label, branch, commit, content.
// Deterministic: timestamps come from the store's sequence, request ids from a counter.
// Foreign owners are 404 (never 403); missing auth is 401; malformed JSON is 400;
// unknown routes are a JSON 404; every reply is JSON with X-GitHub-Request-Id.

import { VIEWER } from './scenario.ts';
import {
  Store,
  globalId,
  isObjectId,
  parseCommit,
  parseTree,
  type Comment,
  type Issue,
  type Label,
  type PullRequest,
  type Repo,
} from './store.ts';
import { objectIdOf } from './git.ts';

export interface RestResponse {
  readonly status: number;
  readonly headers?: Record<string, string>;
  readonly body: Buffer;
}

const EMPTY_BODY = Buffer.alloc(0);

export function jsonBody(status: number, value: unknown, headers?: Record<string, string>): RestResponse {
  return { status, headers, body: Buffer.from(JSON.stringify(value), 'utf8') };
}

export function jsonError(status: number, message: string, extra?: Record<string, unknown>): RestResponse {
  return jsonBody(status, { message, ...(extra ?? {}) });
}

function empty(status: number): RestResponse {
  return { status, body: EMPTY_BODY };
}

export function parseJsonBody(body: Buffer): { ok: true; value: unknown } | { ok: false; response: RestResponse } {
  if (body.length === 0) return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(body.toString('utf8')) as unknown };
  } catch {
    return { ok: false, response: jsonError(400, 'Problems parsing JSON') };
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function asBoolean(value: unknown, fallback: boolean): boolean {
  return typeof value === 'boolean' ? value : fallback;
}

function asString(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

function asInteger(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isInteger(value) ? value : fallback;
}

function userShape(login: string): Record<string, unknown> {
  return {
    login,
    id: 1000,
    node_id: globalId('User', 1000),
    avatar_url: `https://github.local/avatars/${login}`,
    html_url: `https://github.local/${login}`,
    type: 'User',
    site_admin: false,
  };
}

function repoShape(repo: Repo): Record<string, unknown> {
  return {
    id: repo.dbId,
    node_id: globalId('Repository', repo.dbId),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: userShape(repo.owner),
    private: repo.isPrivate,
    html_url: `https://github.local/${repo.owner}/${repo.name}`,
    description: repo.description,
    fork: false,
    url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: repo.createdSeq,
    updated_at: repo.createdSeq,
    pushed_at: repo.createdSeq,
    homepage: null,
    size: 0,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    has_issues: true,
    has_projects: true,
    has_downloads: true,
    has_wiki: true,
    has_pages: false,
    forks_count: 0,
    archived: false,
    disabled: false,
    open_issues_count: [...repo.issues.values()].filter((issue) => issue.state === 'open').length,
    license: null,
    allow_forking: true,
    is_template: false,
    topics: [],
    visibility: repo.isPrivate ? 'private' : 'public',
    forks: 0,
    open_issues: [...repo.issues.values()].filter((issue) => issue.state === 'open').length,
    watchers: 0,
    default_branch: repo.defaultBranch,
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
  };
}

function labelShape(label: Label): Record<string, unknown> {
  return { id: 0, node_id: globalId('Label', label.name), url: '', name: label.name, color: label.color, default: false, description: label.description };
}

function issueShape(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.number,
    node_id: issue.globalId,
    url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}`,
    labels_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/labels{/name}`,
    comments_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/comments`,
    events_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/events`,
    html_url: `https://github.local/${repo.owner}/${repo.name}/issues/${issue.number}`,
    number: issue.number,
    title: issue.title,
    user: userShape(VIEWER.login),
    labels: issue.labels.map((label) => labelShape(label)),
    state: issue.state,
    locked: false,
    assignee: null,
    assignees: [],
    milestone: null,
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    author_association: 'OWNER',
    body: issue.body,
  };
}

/** Serialized issue shape, cached on the issue (the read workload hits one issue 2000x). */
function issueJson(repo: Repo, issue: Issue): string {
  if (issue.cachedJson !== null) return issue.cachedJson;
  const json = JSON.stringify(issueShape(repo, issue));
  issue.cachedJson = json;
  return json;
}

function commentShape(repo: Repo, issue: Issue, comment: Comment): Record<string, unknown> {
  return {
    id: comment.dbId,
    node_id: comment.globalId,
    url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/issues/comments/${comment.dbId}`,
    html_url: `https://github.local/${repo.owner}/${repo.name}/issues/${issue.number}#issuecomment-${comment.dbId}`,
    body: comment.body,
    user: userShape(VIEWER.login),
    created_at: comment.createdAt,
    updated_at: comment.updatedAt,
    author_association: 'OWNER',
  };
}

function pullShape(repo: Repo, pull: PullRequest): Record<string, unknown> {
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  return {
    id: pull.number,
    node_id: pull.globalId,
    url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
    html_url: `https://github.local/${repo.owner}/${repo.name}/pull/${pull.number}`,
    number: pull.number,
    state: pull.state,
    locked: false,
    title: pull.title,
    user: userShape(VIEWER.login),
    body: pull.body,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.state === 'open' ? null : pull.updatedAt,
    merged_at: pull.merged ? pull.updatedAt : null,
    merge_commit_sha: pull.mergeCommitSha,
    assignee: null,
    assignees: [],
    requested_reviewers: [],
    labels: [],
    milestone: null,
    draft: false,
    commits_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}/commits`,
    review_comments_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}/comments`,
    comments_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/issues/${pull.number}/comments`,
    statuses_url: `https://github.local/api/v3/repos/${repo.owner}/${repo.name}/commits/${pull.headSha}/statuses`,
    head: { label: `${VIEWER.login}:${pull.headRef}`, ref: pull.headRef, sha: headTip ?? pull.headSha, user: userShape(VIEWER.login), repo: repoShape(repo) },
    base: { label: `${VIEWER.login}:${pull.baseRef}`, ref: pull.baseRef, sha: baseTip ?? pull.baseSha, user: userShape(VIEWER.login), repo: repoShape(repo) },
    author_association: 'OWNER',
    merged: pull.merged,
    mergeable_state: pull.merged ? 'merged' : pull.state === 'open' ? 'clean' : 'closed',
    merged_by: pull.merged ? userShape(VIEWER.login) : null,
    comments: 0,
    review_comments: 0,
    maintainer_can_modify: false,
    commits: 1,
    additions: 0,
    deletions: 0,
    changed_files: 0,
  };
}

const REPO_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/;

function validationError(message: string, errors: Array<Record<string, unknown>>): RestResponse {
  return jsonBody(422, { message, errors });
}

function paginate<T>(
  url: URL,
  items: readonly T[],
  page: number,
  perPage: number,
): { pageItems: T[]; headers: Record<string, string> } {
  const safePerPage = Math.min(100, Math.max(1, perPage));
  const safePage = Math.max(1, page);
  const totalPages = Math.max(1, Math.ceil(items.length / safePerPage));
  const pageItems = items.slice((safePage - 1) * safePerPage, safePage * safePerPage);
  const pageUrl = (target: number): string => {
    const next = new URL(url.pathname + url.search, url.origin);
    next.searchParams.set('page', String(target));
    next.searchParams.set('per_page', String(safePerPage));
    return next.toString();
  };
  const links: string[] = [];
  if (safePage < totalPages) links.push(`<${pageUrl(safePage + 1)}>; rel="next"`);
  if (totalPages > 1) links.push(`<${pageUrl(totalPages)}>; rel="last"`);
  if (safePage > 1) {
    links.push(`<${pageUrl(1)}>; rel="first"`);
    links.push(`<${pageUrl(safePage - 1)}>; rel="prev"`);
  }
  return { pageItems, headers: links.length ? { link: links.join(', ') } : {} };
}

export interface RestContext {
  readonly store: Store;
  readonly method: string;
  readonly url: URL;
  readonly path: string[];
  readonly body: Buffer;
}

function requireRepo(ctx: RestContext, owner: string, name: string): Repo | RestResponse {
  const repo = ctx.store.getRepo(owner, name);
  if (!repo) return jsonError(404, 'Not Found');
  return repo;
}

function requireViewerRepo(ctx: RestContext, owner: string, name: string): Repo | RestResponse {
  // Any owner other than the viewer is a 404 for this token (never a 403).
  if (owner !== VIEWER.login) return jsonError(404, 'Not Found');
  return requireRepo(ctx, owner, name);
}

export function handleRest(ctx: RestContext): RestResponse {
  const { store, method, url, path } = ctx;
  const query = url.searchParams;
  const page = asInteger(Number(query.get('page') ?? ''), 1) || 1;
  const perPage = asInteger(Number(query.get('per_page') ?? ''), 30) || 30;

  // GET /api/v3/ — the API root.
  if (path.length === 0) {
    if (method !== 'GET') return jsonError(404, 'Not Found');
    return jsonBody(
      200,
      {
        current_user_url: 'https://github.local/api/v3/user',
        current_user_authorizations_html_url: 'https://github.local/settings/applications',
        authorizations_url: 'https://github.local/api/v3/authorizations',
        code_search_url: 'https://github.local/api/v3/search/code?q={query}{&page,per_page,sort,order}',
        commit_search_url: 'https://github.local/api/v3/search/commits?q={query}{&page,per_page,sort,order}',
        emails_url: 'https://github.local/api/v3/user/emails',
        emojis_url: 'https://github.local/api/v3/emojis',
        events_url: 'https://github.local/api/v3/events',
        feeds_url: 'https://github.local/api/v3/feeds',
        followers_url: 'https://github.local/api/v3/user/followers',
        following_url: 'https://github.local/api/v3/user/following{/target}',
        gists_url: 'https://github.local/api/v3/gists{/gist_id}',
        hub_url: 'https://github.local/api/v3/hub',
        issue_search_url: 'https://github.local/api/v3/search/issues?q={query}{&page,per_page,sort,order}',
        issues_url: 'https://github.local/api/v3/issues',
        keys_url: 'https://github.local/api/v3/user/keys',
        label_search_url: 'https://github.local/api/v3/search/labels?q={query}&repository_id={repository_id}{&page,per_page}',
        notifications_url: 'https://github.local/api/v3/notifications',
        organization_url: 'https://github.local/api/v3/orgs/{org}',
        organization_repositories_url: 'https://github.local/api/v3/orgs/{org}/repos{?type,page,per_page,sort}',
        organization_teams_url: 'https://github.local/api/v3/orgs/{org}/teams',
        public_gists_url: 'https://github.local/api/v3/gists/public',
        rate_limit_url: 'https://github.local/api/v3/rate_limit',
        repository_url: 'https://github.local/api/v3/repos/{owner}/{repo}',
        repository_search_url: 'https://github.local/api/v3/search/repositories?q={query}{&page,per_page,sort,order}',
        current_user_repositories_url: 'https://github.local/api/v3/user/repos{?type,page,per_page,sort}',
        starred_url: 'https://github.local/api/v3/user/starred{/owner}{/repo}',
        starred_gists_url: 'https://github.local/api/v3/gists/starred',
        topic_search_url: 'https://github.local/api/v3/search/topics?q={query}{&page,per_page}',
        user_url: 'https://github.local/api/v3/users/{user}',
        user_organizations_url: 'https://github.local/api/v3/users/{user}/orgs',
        user_repositories_url: 'https://github.local/api/v3/users/{user}/repos{?type,page,per_page,sort}',
        user_search_url: 'https://github.local/api/v3/search/users?q={query}{&page,per_page,sort,order}',
      },
      { 'x-oauth-scopes': 'repo, read:org' },
    );
  }

  // GET /api/v3/meta — server metadata.
  if (path.length === 1 && path[0] === 'meta') {
    if (method !== 'GET') return jsonError(404, 'Not Found');
    return jsonBody(200, { installed_version: '1.0.0' });
  }

  // GET /api/v3/user — the viewer.
  if (path.length === 1 && path[0] === 'user') {
    if (method !== 'GET') return jsonError(404, 'Not Found');
    return jsonBody(200, {
      ...userShape(VIEWER.login),
      name: VIEWER.name,
      email: VIEWER.email,
      public_repos: store.listViewerRepos().length,
      followers: 0,
      following: 0,
      created_at: '2025-01-01T00:00:00Z',
      updated_at: '2025-01-01T00:00:00Z',
    });
  }

  // GET /api/v3/users/:login
  if (path.length === 2 && path[0] === 'users') {
    if (method !== 'GET') return jsonError(404, 'Not Found');
    if (path[1] !== VIEWER.login) return jsonError(404, 'Not Found');
    return jsonBody(200, { ...userShape(VIEWER.login), name: VIEWER.name, email: VIEWER.email });
  }

  // /api/v3/user/repos — list + create.
  if (path.length === 2 && path[0] === 'user' && path[1] === 'repos') {
    if (method === 'GET') {
      const repos = store.listViewerRepos();
      const { pageItems, headers } = paginate(url, repos, page, perPage);
      return jsonBody(200, pageItems.map((repo) => repoShape(repo)), headers);
    }
    if (method === 'POST') {
      const parsed = parseJsonBody(ctx.body);
      if (!parsed.ok) return parsed.response;
      const value = parsed.value;
      if (!isRecord(value)) return validationError('Validation Failed', [{ resource: 'Repository', field: 'name', code: 'missing_field' }]);
      const name = asString(value.name);
      if (!REPO_NAME_PATTERN.test(name)) {
        return validationError('Validation Failed', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name is invalid' }]);
      }
      const repo = store.createRepo(name, asBoolean(value.private, false), asString(value.description));
      if (!repo) {
        return validationError('Validation Failed', [{ resource: 'Repository', code: 'custom', field: 'name', message: 'name already exists on this account' }]);
      }
      return jsonBody(201, repoShape(repo));
    }
    return jsonError(404, 'Not Found');
  }

  // /api/v3/repos/:owner/:repo[/...]
  if (path.length >= 3 && path[0] === 'repos') {
    const owner = path[1]!;
    const repoName = path[2]!;
    const rest = path.slice(3);

    // GET/DELETE/PATCH /repos/:owner/:repo
    if (rest.length === 0) {
      if (method === 'GET') {
        const repo = requireViewerRepo(ctx, owner, repoName);
        if (!('refs' in repo)) return repo;
        return jsonBody(200, repoShape(repo));
      }
      if (method === 'DELETE') {
        if (owner !== VIEWER.login) return jsonError(404, 'Not Found');
        if (!store.deleteRepo(owner, repoName)) return jsonError(404, 'Not Found');
        return empty(204);
      }
      if (method === 'PATCH') {
        const repo = requireViewerRepo(ctx, owner, repoName);
        if (!('refs' in repo)) return repo;
        const parsed = parseJsonBody(ctx.body);
        if (!parsed.ok) return parsed.response;
        if (isRecord(parsed.value)) {
          if (typeof parsed.value.description === 'string') repo.description = parsed.value.description;
          if (typeof parsed.value.private === 'boolean') repo.isPrivate = parsed.value.private;
        }
        return jsonBody(200, repoShape(repo));
      }
      return jsonError(404, 'Not Found');
    }

    const repo = requireViewerRepo(ctx, owner, repoName);
    if (!('refs' in repo)) return repo;

    // GET /repos/:owner/:repo/branches/:branch — a single branch.
    if (rest.length === 2 && rest[0] === 'branches') {
      if (method !== 'GET') return jsonError(404, 'Not Found');
      const sha = repo.refs.get(`refs/heads/${rest[1]}`);
      if (!sha) return jsonError(404, 'Not Found');
      return jsonBody(200, {
        name: rest[1],
        commit: { sha, url: `https://github.local/api/v3/repos/${owner}/${repoName}/commits/${sha}` },
        protected: false,
      });
    }

    // Git Data refs: POST /git/refs (create), GET /git/refs (list), GET /git/refs/:ref...
    if (rest.length >= 2 && rest[0] === 'git' && rest[1] === 'refs') {
      const refPath = rest.slice(2).join('/');
      if (method === 'POST' && rest.length === 2) {
        const parsed = parseJsonBody(ctx.body);
        if (!parsed.ok) return parsed.response;
        const value = isRecord(parsed.value) ? parsed.value : {};
        const ref = asString(value.ref);
        const sha = asString(value.sha);
        if (!ref.startsWith('refs/') || !isObjectId(sha)) {
          return validationError('Validation Failed', [{ resource: 'Reference', field: 'ref', code: 'invalid' }]);
        }
        const object = repo.objects.get(sha);
        if (!object || object.type !== 'commit') {
          return validationError('Validation Failed', [{ resource: 'Reference', field: 'sha', code: 'invalid' }]);
        }
        if (repo.refs.has(ref)) {
          return validationError('Validation Failed', [{ resource: 'Reference', field: 'ref', code: 'custom', message: 'Reference already exists' }]);
        }
        repo.refs.set(ref, sha);
        return jsonBody(201, {
          ref,
          node_id: globalId('Ref', repo.dbId, ref),
          url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/refs/${ref}`,
          object: { sha, type: 'commit', url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/commits/${sha}` },
        });
      }
      if (method === 'GET') {
        if (rest.length === 2) {
          const refs = [...repo.refs.entries()]
            .map(([name, sha]) => ({
              ref: name,
              node_id: globalId('Ref', repo.dbId, name),
              url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/refs/${name}`,
              object: { sha, type: 'commit', url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/commits/${sha}` },
            }))
            .sort((a, b) => (a.ref < b.ref ? -1 : 1));
          const { pageItems, headers } = paginate(url, refs, page, perPage);
          return jsonBody(200, pageItems, headers);
        }
        const refPath = `refs/${rest.slice(2).join('/')}`;
        const sha = repo.refs.get(refPath);
        if (!sha) return jsonError(404, 'Not Found');
        return jsonBody(200, {
          ref: refPath,
          node_id: globalId('Ref', repo.dbId, refPath),
          url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/refs/${refPath}`,
          object: { sha, type: 'commit', url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/commits/${sha}` },
        });
      }
      return jsonError(404, 'Not Found');
    }

    // GET /repos/:owner/:repo/branches
    if (rest.length === 1 && rest[0] === 'branches') {
      if (method !== 'GET') return jsonError(404, 'Not Found');
      const branches = [...repo.refs.entries()]
        .filter(([name]) => name.startsWith('refs/heads/'))
        .map(([name, sha]) => ({
          name: name.slice('refs/heads/'.length),
          commit: { sha, url: `https://github.local/api/v3/repos/${owner}/${repoName}/commits/${sha}` },
          protected: false,
        }))
        .sort((a, b) => (a.name < b.name ? -1 : 1));
      const { pageItems, headers } = paginate(url, branches, page, perPage);
      return jsonBody(200, pageItems, headers);
    }

    // GET /repos/:owner/:repo/commits[/:sha]
    if (rest.length >= 1 && rest[0] === 'commits') {
      if (method !== 'GET') return jsonError(404, 'Not Found');
      if (rest.length === 2) {
        const sha = rest[1]!;
        const object = repo.objects.get(sha);
        if (!object || object.type !== 'commit') return jsonError(404, 'Not Found');
        const commit = parseCommit(object.data);
        return jsonBody(200, {
          sha,
          node_id: globalId('Commit', repo.dbId, sha),
          commit: {
            author: { name: commit.author.split(' <')[0], email: commit.author.split('<')[1]?.replace('>', '') ?? '', date: repo.createdSeq },
            committer: { name: commit.committer.split(' <')[0], email: commit.committer.split('<')[1]?.replace('>', '') ?? '', date: repo.createdSeq },
            message: commit.message,
            tree: { sha: commit.tree, url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/trees/${commit.tree}` },
          },
          author: userShape(VIEWER.login),
          committer: userShape(VIEWER.login),
          parents: commit.parents.map((parent) => ({ sha: parent, url: `https://github.local/api/v3/repos/${owner}/${repoName}/commits/${parent}` })),
        });
      }
      const branch = query.get('sha') ?? repo.defaultBranch;
      const tip = repo.refs.get(`refs/heads/${branch}`);
      const commits: Array<Record<string, unknown>> = [];
      if (tip) {
        const seen = new Set<string>();
        const queue = [tip];
        while (queue.length && commits.length < 100) {
          const oid = queue.shift()!;
          if (seen.has(oid)) continue;
          seen.add(oid);
          const object = repo.objects.get(oid);
          if (!object || object.type !== 'commit') continue;
          const commit = parseCommit(object.data);
          commits.push({
            sha: oid,
            node_id: globalId('Commit', repo.dbId, oid),
            commit: {
              author: { name: commit.author.split(' <')[0], email: '', date: repo.createdSeq },
              committer: { name: commit.committer.split(' <')[0], email: '', date: repo.createdSeq },
              message: commit.message,
              tree: { sha: commit.tree, url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/trees/${commit.tree}` },
            },
            author: userShape(VIEWER.login),
            committer: userShape(VIEWER.login),
            parents: commit.parents.map((parent) => ({ sha: parent, url: `https://github.local/api/v3/repos/${owner}/${repoName}/commits/${parent}` })),
          });
          queue.push(...commit.parents);
        }
      }
      const { pageItems, headers } = paginate(url, commits, page, perPage);
      return jsonBody(200, pageItems, headers);
    }

    // GET /repos/:owner/:repo/git/trees/:sha
    if (rest.length === 3 && rest[0] === 'git' && rest[1] === 'trees') {
      if (method !== 'GET') return jsonError(404, 'Not Found');
      const sha = rest[2]!;
      const object = repo.objects.get(sha);
      if (!object || object.type !== 'tree') return jsonError(404, 'Not Found');
      const recursive = query.get('recursive') === '1';
      const entries: Array<Record<string, unknown>> = [];
      const walk = (treeOid: string, prefix: string): void => {
        const treeObject = repo.objects.get(treeOid);
        if (!treeObject || treeObject.type !== 'tree') return;
        for (const entry of parseTree(treeObject.data)) {
          const pathOut = prefix ? `${prefix}/${entry.name}` : entry.name;
          entries.push({
            path: pathOut,
            mode: entry.mode,
            type: entry.mode === '40000' ? 'tree' : 'blob',
            sha: entry.oid,
            size: entry.mode === '40000' ? undefined : (repo.objects.get(entry.oid)?.data.length ?? 0),
            url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/${entry.mode === '40000' ? 'trees' : 'blobs'}/${entry.oid}`,
          });
          if (recursive && entry.mode === '40000') walk(entry.oid, pathOut);
        }
      };
      walk(sha, '');
      return jsonBody(200, { sha, url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/trees/${sha}`, tree: entries, truncated: false });
    }

    // GET /repos/:owner/:repo/git/blobs/:sha
    if (rest.length === 3 && rest[0] === 'git' && rest[1] === 'blobs') {
      if (method !== 'GET') return jsonError(404, 'Not Found');
      const sha = rest[2]!;
      const object = repo.objects.get(sha);
      if (!object || object.type !== 'blob') return jsonError(404, 'Not Found');
      return jsonBody(200, {
        sha,
        node_id: globalId('Blob', repo.dbId, sha),
        size: object.data.length,
        url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/blobs/${sha}`,
        content: object.data.toString('base64'),
        encoding: 'base64',
      });
    }

    // /repos/:owner/:repo/contents/:path... — GET, PUT, DELETE
    if (rest.length >= 1 && rest[0] === 'contents') {
      const filePath = rest.slice(1).join('/');
      if (method === 'GET') {
        const branch = query.get('ref') ?? repo.defaultBranch;
        const tip = repo.refs.get(`refs/heads/${branch}`);
        if (!tip) return jsonError(404, 'Not Found');
        const head = repo.objects.get(tip);
        if (!head || head.type !== 'commit') return jsonError(404, 'Not Found');
        const flat = store.flattenTree(repo, parseCommit(head.data).tree);
        const blobOid = flat.get(filePath);
        if (blobOid) {
          const blob = repo.objects.get(blobOid);
          if (!blob || blob.type !== 'blob') return jsonError(404, 'Not Found');
          return jsonBody(200, {
            type: 'file',
            encoding: 'base64',
            size: blob.data.length,
            name: filePath.split('/').pop() ?? filePath,
            path: filePath,
            content: blob.data.toString('base64'),
            sha: blobOid,
            url: `https://github.local/api/v3/repos/${owner}/${repoName}/contents/${filePath}`,
            html_url: `https://github.local/${owner}/${repoName}/blob/${branch}/${filePath}`,
            git_url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/blobs/${blobOid}`,
            download_url: `https://github.local/${owner}/${repoName}/raw/${branch}/${filePath}`,
          });
        }
        // Directory listing: immediate children of the path ('' = root).
        const prefix = filePath === '' ? '' : `${filePath}/`;
        const children = new Map<string, { type: string; sha: string; size: number }>();
        for (const [candidate, oid] of flat) {
          if (!candidate.startsWith(prefix)) continue;
          const remainder = candidate.slice(prefix.length);
          if (!remainder) continue;
          const headSegment = remainder.split('/')[0]!;
          if (remainder.includes('/')) {
            if (!children.has(headSegment)) {
              // The subtree's tree oid is found by walking the head tree.
              const dirOid = findTreeOid(repo, tip, filePath === '' ? headSegment : `${filePath}/${headSegment}`);
              if (dirOid) children.set(headSegment, { type: 'dir', sha: dirOid, size: 0 });
            }
          } else {
            children.set(headSegment, { type: 'file', sha: oid, size: repo.objects.get(oid)?.data.length ?? 0 });
          }
        }
        if (children.size === 0) return jsonError(404, 'Not Found');
        const listing = [...children.entries()]
          .map(([name, value]) => ({
            type: value.type,
            size: value.size,
            name,
            path: prefix + name,
            sha: value.sha,
            url: `https://github.local/api/v3/repos/${owner}/${repoName}/contents/${prefix}${name}`,
            html_url: `https://github.local/${owner}/${repoName}/tree/${branch}/${prefix}${name}`,
            git_url: `https://github.local/api/v3/repos/${owner}/${repoName}/git/${value.type === 'dir' ? 'trees' : 'blobs'}/${value.sha}`,
            download_url: value.type === 'file' ? `https://github.local/${owner}/${repoName}/raw/${branch}/${prefix}${name}` : null,
          }))
          .sort((a, b) => (a.name < b.name ? -1 : 1));
        return jsonBody(200, listing);
      }
      if (method === 'PUT' || method === 'DELETE') {
        const parsed = parseJsonBody(ctx.body);
        if (!parsed.ok) return parsed.response;
        const value = isRecord(parsed.value) ? parsed.value : {};
        const branch = asString(value.branch, repo.defaultBranch);
        const tip = repo.refs.get(`refs/heads/${branch}`);
        const flat = tip ? store.flattenTree(repo, parseCommit(repo.objects.get(tip)?.data ?? Buffer.alloc(0)).tree) : new Map<string, string>();
        const existing = flat.get(filePath);
        if (method === 'PUT') {
          const contentB64 = asString(value.content);
          if (!contentB64) return validationError('Validation Failed', [{ resource: 'Content', field: 'content', code: 'missing_field' }]);
          const content = Buffer.from(contentB64, 'base64');
          if (existing && typeof value.sha === 'string' && value.sha !== existing) {
            return jsonBody(409, { message: 'sha does not match the current blob' });
          }
          if (!existing && typeof value.sha === 'string' && value.sha !== '') {
            return jsonBody(409, { message: 'sha does not match the current blob' });
          }
          const message = asString(value.message, `Update ${filePath}`);
          const sha = store.createCommitOnBranch(repo, branch, filePath, content, message);
          const blobOid = objectIdOf('blob', content);
          const status = existing ? 200 : 201;
          return jsonBody(status, {
            content: { name: filePath.split('/').pop() ?? filePath, path: filePath, sha: blobOid, size: content.length, url: '', html_url: '', git_url: '', download_url: '', type: 'file', content: contentB64, encoding: 'base64' },
            commit: { sha, node_id: globalId('Commit', repo.dbId, sha), message, author: { name: VIEWER.name, email: VIEWER.email, date: repo.createdSeq }, committer: { name: VIEWER.name, email: VIEWER.email, date: repo.createdSeq } },
          });
        }
        // DELETE
        if (!existing) return jsonError(404, 'Not Found');
        if (typeof value.sha === 'string' && value.sha !== existing) {
          return jsonBody(409, { message: 'sha does not match the current blob' });
        }
        const message = asString(value.message, `Delete ${filePath}`);
        const remaining = new Map(flat);
        remaining.delete(filePath);
        // One commit whose tree is the flat map without the file.
        const rebuilt = rebuildBranchWithout(repo, branch, remaining, message);
        return jsonBody(200, {
          content: null,
          commit: { sha: rebuilt, node_id: globalId('Commit', repo.dbId, rebuilt), message, author: { name: VIEWER.name, email: VIEWER.email, date: repo.createdSeq }, committer: { name: VIEWER.name, email: VIEWER.email, date: repo.createdSeq } },
        });
      }
      return jsonError(404, 'Not Found');
    }

    // /repos/:owner/:repo/issues[/:number[/comments|/labels]] and /labels
    if (rest.length >= 1 && rest[0] === 'issues') {
      if (rest.length === 1) {
        if (method === 'GET') {
          let issues = [...repo.issues.values()];
          const state = query.get('state') ?? 'open';
          if (state !== 'all') issues = issues.filter((issue) => issue.state === state);
          const labelsParam = query.get('labels');
          if (labelsParam) {
            const wanted = labelsParam.split(',').map((item) => item.trim()).filter(Boolean);
            issues = issues.filter((issue) => wanted.every((name) => issue.labels.some((label) => label.name === name)));
          }
          // sort=created&direction=asc orders by creation (number asc); the default is newest first.
          const direction = query.get('direction') ?? 'desc';
          issues.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
          const { pageItems, headers } = paginate(url, issues, page, perPage);
          return jsonBody(200, pageItems.map((issue) => JSON.parse(issueJson(repo, issue)) as unknown), headers);
        }
        if (method === 'POST') {
          const parsed = parseJsonBody(ctx.body);
          if (!parsed.ok) return parsed.response;
          const value = isRecord(parsed.value) ? parsed.value : {};
          const title = asString(value.title);
          if (!title) return validationError('Validation Failed', [{ resource: 'Issue', field: 'title', code: 'missing_field' }]);
          const labels = Array.isArray(value.labels) ? value.labels.filter((item): item is string => typeof item === 'string') : [];
          // An unknown label is a 422 (GitHub rejects labels that do not exist on the repo).
          const unknown = labels.find((name) => !repo.labels.has(name));
          if (unknown !== undefined) {
            return validationError('Validation Failed', [{ resource: 'Issue', code: 'custom', field: 'labels', message: `label ${unknown} does not exist` }]);
          }
          const issue = store.createIssue(repo, title, asString(value.body), labels);
          return { status: 201, body: Buffer.from(issueJson(repo, issue), 'utf8') };
        }
        return jsonError(404, 'Not Found');
      }
      const number = Number(rest[1]);
      if (!Number.isInteger(number)) return jsonError(404, 'Not Found');
      const issue = repo.issues.get(number);
      if (!issue) return jsonError(404, 'Not Found');
      if (rest.length === 2) {
        if (method === 'GET') return { status: 200, body: Buffer.from(issueJson(repo, issue), 'utf8') };
        if (method === 'PATCH') {
          const parsed = parseJsonBody(ctx.body);
          if (!parsed.ok) return parsed.response;
          const value = isRecord(parsed.value) ? parsed.value : {};
          if (typeof value.title === 'string') issue.title = value.title;
          if (typeof value.body === 'string') issue.body = value.body;
          if (typeof value.state === 'string' && (value.state === 'open' || value.state === 'closed')) {
            issue.state = value.state;
            issue.closedAt = value.state === 'closed' ? store.seqTimestamp() : null;
          }
          issue.updatedAt = store.seqTimestamp();
          store.touchIssue(issue);
          return { status: 200, body: Buffer.from(issueJson(repo, issue), 'utf8') };
        }
        return jsonError(404, 'Not Found');
      }
      if (rest.length === 3 && rest[2] === 'comments') {
        if (method === 'GET') {
          const { pageItems, headers } = paginate(url, issue.comments, page, perPage);
          return jsonBody(200, pageItems.map((comment) => commentShape(repo, issue, comment)), headers);
        }
        if (method === 'POST') {
          const parsed = parseJsonBody(ctx.body);
          if (!parsed.ok) return parsed.response;
          const value = isRecord(parsed.value) ? parsed.value : {};
          const bodyText = asString(value.body);
          if (!bodyText) return validationError('Validation Failed', [{ resource: 'IssueComment', field: 'body', code: 'missing_field' }]);
          const comment = store.addComment(repo, issue, bodyText);
          return jsonBody(201, commentShape(repo, issue, comment));
        }
        return jsonError(404, 'Not Found');
      }
      if (rest.length === 3 && rest[2] === 'labels') {
        if (method === 'GET') {
          return jsonBody(200, issue.labels.map((label) => labelShape(label)));
        }
        if (method === 'POST' || method === 'PUT') {
          const parsed = parseJsonBody(ctx.body);
          if (!parsed.ok) return parsed.response;
          const value = parsed.value;
          const names: string[] = [];
          if (Array.isArray(value)) {
            for (const item of value) if (typeof item === 'string') names.push(item);
          } else if (isRecord(value) && Array.isArray(value.labels)) {
            for (const item of value.labels) if (typeof item === 'string') names.push(item);
          }
          issue.labels = names
            .map((name) => repo.labels.get(name))
            .filter((label): label is Label => label !== undefined)
            .map((label) => ({ ...label }));
          issue.updatedAt = store.seqTimestamp();
          store.touchIssue(issue);
          return jsonBody(200, issue.labels.map((label) => labelShape(label)));
        }
        return jsonError(404, 'Not Found');
      }
      return jsonError(404, 'Not Found');
    }

    // /repos/:owner/:repo/labels — list + create (idempotent: an existing name returns 201).
    if (rest.length === 1 && rest[0] === 'labels') {
      if (method === 'GET') {
        const { pageItems, headers } = paginate(url, [...repo.labels.values()], page, perPage);
        return jsonBody(200, pageItems.map((label) => labelShape(label)), headers);
      }
      if (method === 'POST') {
        const parsed = parseJsonBody(ctx.body);
        if (!parsed.ok) return parsed.response;
        const value = isRecord(parsed.value) ? parsed.value : {};
        const name = asString(value.name);
        if (!name) return validationError('Validation Failed', [{ resource: 'Label', field: 'name', code: 'missing_field' }]);
        const existing = repo.labels.get(name);
        if (existing) return jsonBody(201, labelShape(existing));
        const label: Label = { name, color: asString(value.color, 'ededed'), description: asString(value.description) };
        repo.labels.set(name, label);
        return jsonBody(201, labelShape(label));
      }
      return jsonError(404, 'Not Found');
    }

    // /repos/:owner/:repo/pulls[/:number[/merge|/files|/commits]]
    if (rest.length >= 1 && rest[0] === 'pulls') {
      if (rest.length === 1) {
        if (method === 'GET') {
          let pulls = [...repo.pulls.values()];
          const state = query.get('state') ?? 'open';
          if (state !== 'all') pulls = pulls.filter((pull) => pull.state === state);
          pulls.sort((a, b) => b.number - a.number);
          const { pageItems, headers } = paginate(url, pulls, page, perPage);
          return jsonBody(200, pageItems.map((pull) => pullShape(repo, pull)), headers);
        }
        if (method === 'POST') {
          const parsed = parseJsonBody(ctx.body);
          if (!parsed.ok) return parsed.response;
          const value = isRecord(parsed.value) ? parsed.value : {};
          const title = asString(value.title);
          if (!title) return validationError('Validation Failed', [{ resource: 'PullRequest', field: 'title', code: 'missing_field' }]);
          const head = asString(value.head).split(':').pop() ?? '';
          const base = asString(value.base).split(':').pop() ?? '';
          const created = store.createPull(repo, title, asString(value.body), head, base);
          if (!created.ok) return validationError('Validation Failed', [{ resource: 'PullRequest', field: 'head', code: 'custom', message: created.message }]);
          return jsonBody(201, pullShape(repo, created.pull));
        }
        return jsonError(404, 'Not Found');
      }
      const number = Number(rest[1]);
      if (!Number.isInteger(number)) return jsonError(404, 'Not Found');
      const pull = repo.pulls.get(number);
      if (!pull) return jsonError(404, 'Not Found');
      if (rest.length === 2) {
        if (method === 'GET') return jsonBody(200, pullShape(repo, pull));
        if (method === 'PATCH') {
          const parsed = parseJsonBody(ctx.body);
          if (!parsed.ok) return parsed.response;
          const value = isRecord(parsed.value) ? parsed.value : {};
          if (typeof value.title === 'string') pull.title = value.title;
          if (typeof value.body === 'string') pull.body = value.body;
          if (typeof value.state === 'string' && (value.state === 'open' || value.state === 'closed')) pull.state = value.state;
          pull.updatedAt = store.seqTimestamp();
          return jsonBody(200, pullShape(repo, pull));
        }
        return jsonError(404, 'Not Found');
      }
      if (rest.length === 3 && rest[2] === 'merge') {
        if (method !== 'PUT' && method !== 'POST') return jsonError(404, 'Not Found');
        const merged = store.mergePull(repo, pull);
        if (!merged.ok) return jsonBody(405, { message: merged.message });
        return jsonBody(200, { merged: true, sha: merged.sha, message: 'Pull Request successfully merged' });
      }
      if (rest.length === 3 && rest[2] === 'files') {
        if (method !== 'GET') return jsonError(404, 'Not Found');
        const files = store.pullFiles(repo, pull).map((file) => {
          const diff = fileLineDiff(store, repo, pull, file.filename, file.status);
          return {
            sha: pull.headSha,
            filename: file.filename,
            status: file.status,
            additions: diff.additions,
            deletions: diff.deletions,
            changes: diff.additions + diff.deletions,
            blob_url: `https://github.local/${owner}/${repoName}/blob/${pull.headSha}/${file.filename}`,
            raw_url: `https://github.local/${owner}/${repoName}/raw/${pull.headSha}/${file.filename}`,
            contents_url: `https://github.local/api/v3/repos/${owner}/${repoName}/contents/${file.filename}?ref=${pull.headSha}`,
          };
        });
        const { pageItems, headers } = paginate(url, files, page, perPage);
        return jsonBody(200, pageItems, headers);
      }
      if (rest.length === 3 && rest[2] === 'commits') {
        if (method !== 'GET') return jsonError(404, 'Not Found');
        const commits: Array<Record<string, unknown>> = [];
        const seen = new Set<string>();
        const queue = [pull.headSha];
        while (queue.length) {
          const oid = queue.shift()!;
          if (seen.has(oid) || oid === pull.baseSha) continue;
          seen.add(oid);
          const object = repo.objects.get(oid);
          if (!object || object.type !== 'commit') continue;
          const commit = parseCommit(object.data);
          commits.push({ sha: oid, commit: { message: commit.message, tree: { sha: commit.tree } }, parents: commit.parents.map((parent) => ({ sha: parent })) });
          queue.push(...commit.parents);
        }
        return jsonBody(200, commits);
      }
      return jsonError(404, 'Not Found');
    }

    return jsonError(404, 'Not Found');
  }

  return jsonError(404, 'Not Found');
}

/** Line additions/deletions for one pull-request file (multiset line diff). */
function fileLineDiff(
  store: Store,
  repo: Repo,
  pull: PullRequest,
  filename: string,
  status: string,
): { additions: number; deletions: number } {
  const headSha = store.currentHeadSha(repo, pull);
  const baseSha = store.currentBaseSha(repo, pull);
  const headCommit = repo.objects.get(headSha);
  const baseCommit = repo.objects.get(baseSha);
  const headTree = headCommit && headCommit.type === 'commit' ? store.flattenTree(repo, parseCommit(headCommit.data).tree) : new Map<string, string>();
  const baseTree = baseCommit && baseCommit.type === 'commit' ? store.flattenTree(repo, parseCommit(baseCommit.data).tree) : new Map<string, string>();
  const blobLines = (oid: string | undefined): string[] => {
    if (!oid) return [];
    const blob = repo.objects.get(oid);
    if (!blob || blob.type !== 'blob') return [];
    const lines = blob.data.toString('utf8').split('\n');
    if (lines.length && lines[lines.length - 1] === '') lines.pop();
    return lines;
  };
  const headLines = blobLines(headTree.get(filename));
  const baseLines = blobLines(baseTree.get(filename));
  if (status === 'added') return { additions: Math.max(1, headLines.length), deletions: 0 };
  if (status === 'removed') return { additions: 0, deletions: Math.max(1, baseLines.length) };
  const count = (lines: string[]): Map<string, number> => {
    const out = new Map<string, number>();
    for (const line of lines) out.set(line, (out.get(line) ?? 0) + 1);
    return out;
  };
  const headCount = count(headLines);
  const baseCount = count(baseLines);
  let additions = 0;
  for (const [line, n] of headCount) additions += Math.max(0, n - (baseCount.get(line) ?? 0));
  let deletions = 0;
  for (const [line, n] of baseCount) deletions += Math.max(0, n - (headCount.get(line) ?? 0));
  // A changed file with identical line multisets (pure reorder) still changed.
  if (additions === 0 && deletions === 0) additions = 1;
  return { additions, deletions };
}

/** Walk the head tree to find a subtree's oid (used for directory listings). */
function findTreeOid(repo: Repo, tip: string, dirPath: string): string | null {
  const head = repo.objects.get(tip);
  if (!head || head.type !== 'commit') return null;
  let current = parseCommit(head.data).tree;
  for (const segment of dirPath.split('/').filter(Boolean)) {
    const object = repo.objects.get(current);
    if (!object || object.type !== 'tree') return null;
    const entry = parseTree(object.data).find((item) => item.name === segment && item.mode === '40000');
    if (!entry) return null;
    current = entry.oid;
  }
  return current;
}

/** Rebuild a branch's tree from a flat map and commit it (contents DELETE). */
function rebuildBranchWithout(repo: Repo, branch: string, flat: Map<string, string>, message: string): string {
  const ref = `refs/heads/${branch}`;
  const current = repo.refs.get(ref);
  const parents: string[] = current ? [current] : [];
  // Rebuild the tree from the flat map by reusing createCommitOnBranch's tree builder
  // through a synthetic empty-then-fill: instead, build the tree directly here.
  const treeOid = buildTreeFromFlatLocal(repo, flat);
  const timestamp = 1_700_000_000 + repo.commitCounter + 1;
  repo.commitCounter += 1;
  const commitText =
    `tree ${treeOid}\n` +
    parents.map((parent) => `parent ${parent}\n`).join('') +
    `author ${VIEWER.name} <${VIEWER.email}> ${timestamp} +0000\n` +
    `committer ${VIEWER.name} <${VIEWER.email}> ${timestamp} +0000\n\n${message}`;
  const commitData = Buffer.from(commitText, 'utf8');
  const sha = objectIdOf('commit', commitData);
  repo.objects.set(sha, { type: 'commit', data: commitData });
  repo.refs.set(ref, sha);
  return sha;
}

function buildTreeFromFlatLocal(repo: Repo, flat: Map<string, string>): string {
  // Build nested trees bottom-up from the flat path map.
  const dirs = new Map<string, Map<string, { mode: string; oid: string }>>();
  dirs.set('', new Map());
  for (const [path, oid] of flat) {
    const parts = path.split('/');
    let at = '';
    for (let i = 0; i < parts.length - 1; i += 1) {
      const part = parts[i]!;
      const next = at ? `${at}/${part}` : part;
      if (!dirs.has(next)) dirs.set(next, new Map());
      const parent = dirs.get(at)!;
      if (!parent.has(part)) parent.set(part, { mode: '40000', oid: '' });
      at = next;
    }
    dirs.get(at)!.set(parts[parts.length - 1]!, { mode: '100644', oid });
  }
  const build = (dir: string): string => {
    const entries: Array<{ mode: string; name: string; oid: string }> = [];
    for (const [name, value] of dirs.get(dir)!) {
      if (value.mode === '40000') {
        const child = dir ? `${dir}/${name}` : name;
        entries.push({ mode: '40000', name, oid: build(child) });
      } else {
        entries.push({ mode: value.mode, name, oid: value.oid });
      }
    }
    entries.sort((a, b) => {
      const aKey = a.mode === '40000' ? `${a.name}/` : a.name;
      const bKey = b.mode === '40000' ? `${b.name}/` : b.name;
      return aKey < bKey ? -1 : aKey > bKey ? 1 : 0;
    });
    const parts: Buffer[] = [];
    for (const entry of entries) {
      parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
      parts.push(Buffer.from(entry.oid, 'hex'));
    }
    const bytes = Buffer.concat(parts);
    const oid = objectIdOf('tree', bytes);
    repo.objects.set(oid, { type: 'tree', data: bytes });
    return oid;
  };
  return build('');
}
