import { initRouter } from "./router.js";
import "./style.css";

initRouter({
  dashboard: { mount: (container) => import("./views/dashboard.js").then((m) => m.mount(container)) },
  project: { mount: (container, route) => import("./views/project.js").then((m) => m.mount(container, route)) },
});
