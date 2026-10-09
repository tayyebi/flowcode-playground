// A small hash router with two routes: the project dashboard (default) and a
// single project's workspace.
//
// Unknown hashes — including the anonymous playground's old `#/playground`
// and bare `#src=...` permalinks, which no longer resolve to anything —
// land on the dashboard rather than a blank page.

const views = {
  dashboard: document.getElementById("view-dashboard"),
  project: document.getElementById("view-project"),
};

let current = null; // { name, unmount? }

function parseRoute(hash) {
  const path = hash.replace(/^#\/?/, "");
  const parts = path.split("/").filter(Boolean);

  if (parts[0] === "projects" && parts[1]) {
    return { name: "project", projectId: parts[1], tab: parts[2] };
  }
  return { name: "dashboard" };
}

// initRouter wires hashchange to mount/unmount the matching view.
// handlers = { dashboard: {mount, unmount}, project: {...} }
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
