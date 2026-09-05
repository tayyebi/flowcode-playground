// A small hash router with three routes: the playground (default), the
// project dashboard, and a single project's workspace.
//
// One hash shape predates this router and must keep working unchanged:
// share.js writes bare `#src=...` permalinks with no route prefix, because
// that fragment is never sent to the server and the format was fixed before
// Projects existed. Any hash starting with `src=` is treated as the
// playground route for exactly that reason.

const views = {
  playground: document.getElementById("view-playground"),
  dashboard: document.getElementById("view-dashboard"),
  project: document.getElementById("view-project"),
};

let current = null; // { name, unmount? }

function parseRoute(hash) {
  if (hash.startsWith("#src=")) return { name: "playground" };

  const path = hash.replace(/^#\/?/, "");
  const parts = path.split("/").filter(Boolean);

  if (parts.length === 0 || parts[0] === "playground") return { name: "playground" };
  if (parts[0] === "dashboard") return { name: "dashboard" };
  if (parts[0] === "projects" && parts[1]) {
    return { name: "project", projectId: parts[1], tab: parts[2] };
  }
  return { name: "playground" };
}

// initRouter wires hashchange to mount/unmount the matching view.
// handlers = { playground: {mount, unmount}, dashboard: {...}, project: {...} }
// Each mount(container, route) may return an unmount function.
export function initRouter(handlers) {
  async function apply() {
    const route = parseRoute(window.location.hash);

    if (current && current.name !== route.name && typeof current.unmount === "function") {
      current.unmount();
    }

    for (const [name, el] of Object.entries(views)) {
      el.hidden = name !== route.name;
    }
    for (const link of document.querySelectorAll(".topnav-link")) {
      link.classList.toggle("active", link.dataset.route === route.name);
    }

    const handler = handlers[route.name];
    let unmount;
    if (handler?.mount) {
      unmount = await handler.mount(views[route.name], route);
    }
    current = { name: route.name, unmount };
  }

  window.addEventListener("hashchange", apply);
  apply();
}
