// Fetch wrapper for the Projects API (/api/projects/...).
//
// Management routes are gated by a single shared admin token when the server
// has one configured (PLAYGROUND_ADMIN_TOKEN) — see server/adminauth.go. The
// browser holds it as an HttpOnly cookie set by /api/admin/login, so this
// module never touches the token itself; it just reacts to a 401 by asking
// the caller to prompt for one and retrying once.

class ApiError extends Error {
  constructor(status, body) {
    super(body?.error || `request failed (${status})`);
    this.status = status;
    this.body = body;
  }
}

async function request(method, path, body) {
  const response = await fetch(path, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: "same-origin",
  });

  if (response.status === 204) return null;

  let parsed = null;
  const text = await response.text();
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = { raw: text };
    }
  }

  if (!response.ok) throw new ApiError(response.status, parsed);
  return parsed;
}

// withAdminRetry lets a view do: `await withAdminRetry(() => api.listProjects())`
// so a 401 pops the token prompt once and retries, instead of every call site
// needing to know about the login flow.
export async function withAdminRetry(fn, promptForToken) {
  try {
    return await fn();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401 && promptForToken) {
      const token = await promptForToken();
      if (!token) throw err;
      await request("POST", "/api/admin/login", { token });
      return await fn();
    }
    throw err;
  }
}

export const api = {
  listProjects: () => request("GET", "/api/projects"),
  createProject: (name, description) => request("POST", "/api/projects", { name, description }),
  getProject: (id) => request("GET", `/api/projects/${id}`),
  updateProject: (id, fields) => request("PATCH", `/api/projects/${id}`, fields),
  deleteProject: (id) => request("DELETE", `/api/projects/${id}`),

  listFiles: (id) => request("GET", `/api/projects/${id}/files`),
  saveFile: (id, name, content) => request("PUT", `/api/projects/${id}/files/${encodeURIComponent(name)}`, { content }),
  deleteFile: (id, name) => request("DELETE", `/api/projects/${id}/files/${encodeURIComponent(name)}`),
  runFile: (id, name) => request("POST", `/api/projects/${id}/files/${encodeURIComponent(name)}/run`),

  listVersions: (id) => request("GET", `/api/projects/${id}/versions`),
  createVersion: (id, label) => request("POST", `/api/projects/${id}/versions`, { label }),
  restoreVersion: (id, number) => request("POST", `/api/projects/${id}/versions/${number}/restore`),

  listDeployments: (id) => request("GET", `/api/projects/${id}/deployments`),
  createDeployment: (id, fileName, versionId) => request("POST", `/api/projects/${id}/deployments`, { fileName, versionId }),
  setDeploymentEnabled: (id, depId, enabled) => request("PATCH", `/api/projects/${id}/deployments/${depId}`, { enabled }),
  deleteDeployment: (id, depId) => request("DELETE", `/api/projects/${id}/deployments/${depId}`),

  listTriggers: (id) => request("GET", `/api/projects/${id}/triggers`),
  createTrigger: (id, fields) => request("POST", `/api/projects/${id}/triggers`, fields),
  setTriggerEnabled: (id, trigId, enabled) => request("PATCH", `/api/projects/${id}/triggers/${trigId}`, { enabled }),
  deleteTrigger: (id, trigId) => request("DELETE", `/api/projects/${id}/triggers/${trigId}`),

  listExecutions: (id) => request("GET", `/api/projects/${id}/executions`),

  listKV: (id) => request("GET", `/api/projects/${id}/kv`),
  deleteKV: (id, key) => request("DELETE", `/api/projects/${id}/kv/${encodeURIComponent(key)}`),
};

export { ApiError };
