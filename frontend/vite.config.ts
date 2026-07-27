import { defineConfig, type Plugin } from "vite";
import { cpSync, existsSync, readFileSync } from "node:fs";
import { join, normalize } from "node:path";
import { fileURLToPath } from "node:url";

const fontsDir = fileURLToPath(
  new URL("./node_modules/@excalidraw/excalidraw/dist/prod/fonts", import.meta.url),
);
const outFontsDir = fileURLToPath(new URL("./dist/excalidraw-assets/fonts", import.meta.url));

// Excalidraw resolves its canvas fonts at runtime from EXCALIDRAW_ASSET_PATH
// (set in main.ts) as `<base>fonts/<Family>/<file>.woff2` — it does NOT
// bundle them, and unset it would fall back to a CDN. Ship them ourselves so
// nothing leaves the machine (the app's no-egress rule): copy into dist for
// the embedded build, serve straight from node_modules in dev.
function excalidrawAssets(): Plugin {
  return {
    name: "excalidraw-assets",
    closeBundle() {
      cpSync(fontsDir, outFontsDir, { recursive: true });
    },
    configureServer(server) {
      server.middlewares.use("/excalidraw-assets/fonts", (req, res, next) => {
        const rel = normalize(decodeURIComponent((req.url ?? "").split("?")[0] ?? ""));
        const file = join(fontsDir, rel);
        if (!file.startsWith(fontsDir) || !existsSync(file)) {
          next();
          return;
        }
        res.setHeader("Content-Type", "font/woff2");
        res.end(readFileSync(file));
      });
    },
  };
}

export default defineConfig({
  plugins: [excalidrawAssets()],
  server: {
    // Vite would otherwise bind [::1] only, so http://127.0.0.1:5173 — the form
    // every other address in this project takes — would just hang. Same
    // loopback-only guarantee as the Go server, spelled the same way.
    host: "127.0.0.1",
    proxy: {
      // The DEV binary (`make dev-api`), not the launchd instance on 4777.
      // Pointing dev at 4777 means editing the real board through a frontend
      // that can't see your Go changes — the binary there is whatever was last
      // built and installed.
      "/api": {
        target: "http://127.0.0.1:4778",
        changeOrigin: true,
        // The Go server's hostGuard refuses browser writes carrying a foreign
        // Origin, and through this proxy every write IS foreign (5173 → 4778).
        // The proxy is a trusted local hop, so drop the header: an Origin-less
        // request is treated like curl/MCP — exactly what a proxied dev
        // request is. changeOrigin above already fixes the Host half.
        configure(proxy) {
          proxy.on("proxyReq", (proxyReq) => proxyReq.removeHeader("origin"));
        },
      },
    },
  },
  build: {
    outDir: "dist",
  },
});
