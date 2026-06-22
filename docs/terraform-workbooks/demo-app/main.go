// Command spinifex-demo is the demo web app served by the EKS Terraform
// workbooks. It renders a Spinifex-themed page reporting the pod, node, cluster,
// and region that answered the request, plus a hit counter.
//
// The counter is held in memory by default. When DATA_DIR points at a writable
// directory (an EBS-CSI PersistentVolume in the gitops workbook), the counter is
// persisted there and survives pod restarts — demonstrating Viperblock-backed
// durable storage. All configuration comes from the environment so a single
// image serves every workbook tier.
package main

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// instance captures the per-pod facts surfaced on the page, populated from the
// downward API and the deployment environment.
type instance struct {
	Pod       string
	Node      string
	Namespace string
	Cluster   string
	Region    string
	Title     string
	Persisted bool
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// counter tracks page hits, optionally persisting to dir/hits so the count
// survives a pod restart when backed by a PersistentVolume.
type counter struct {
	mu   sync.Mutex
	n    int64
	path string
}

func newCounter(dir string) *counter {
	c := &counter{}
	if dir == "" {
		return c
	}
	c.path = filepath.Join(dir, "hits")
	if b, err := os.ReadFile(c.path); err == nil {
		if n, perr := strconv.ParseInt(string(b), 10, 64); perr == nil {
			c.n = n
		}
	}
	return c
}

func (c *counter) inc() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if c.path != "" {
		// Best-effort persist; a read-only or missing volume must not 500 the page.
		if err := os.WriteFile(c.path, []byte(strconv.FormatInt(c.n, 10)), 0o644); err != nil {
			slog.Warn("persist counter failed", "path", c.path, "err", err)
		}
	}
	return c.n
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	dataDir := os.Getenv("DATA_DIR")
	hits := newCounter(dataDir)

	base := instance{
		Pod:       env("POD_NAME", "unknown"),
		Node:      env("NODE_NAME", "unknown"),
		Namespace: env("POD_NAMESPACE", "default"),
		Cluster:   env("CLUSTER_NAME", "spinifex-eks"),
		Region:    env("AWS_REGION", env("AWS_DEFAULT_REGION", "ap-southeast-2")),
		Title:     env("APP_TITLE", "Spinifex EKS"),
		Persisted: dataDir != "",
	}

	tmpl := template.Must(template.New("page").Parse(pageHTML))

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"pod":       base.Pod,
			"node":      base.Node,
			"namespace": base.Namespace,
			"cluster":   base.Cluster,
			"region":    base.Region,
			"hits":      hits.inc(),
			"persisted": base.Persisted,
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		view := struct {
			instance
			Hits int64
			Now  string
		}{base, hits.inc(), time.Now().UTC().Format("2006-01-02 15:04:05 MST")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, view); err != nil {
			slog.Error("render failed", "err", err)
		}
	})

	addr := ":" + env("PORT", "8080")
	slog.Info("spinifex-demo listening", "addr", addr, "cluster", base.Cluster, "persisted", base.Persisted)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
  :root {
    --bg: #0b0e14; --panel: #131823; --line: #232a39;
    --fg: #e6edf3; --muted: #8b97a7; --accent: #f5b301; --accent2: #3ddc97;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; background:
      radial-gradient(1200px 600px at 80% -10%, rgba(245,179,1,.10), transparent 60%),
      radial-gradient(900px 500px at -10% 110%, rgba(61,220,151,.08), transparent 55%), var(--bg);
    color: var(--fg); font: 15px/1.55 ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
    display: flex; align-items: center; justify-content: center; padding: 32px;
  }
  .card {
    width: 100%; max-width: 640px; background: var(--panel);
    border: 1px solid var(--line); border-radius: 16px; overflow: hidden;
    box-shadow: 0 20px 60px rgba(0,0,0,.45);
  }
  .head { padding: 28px 32px; border-bottom: 1px solid var(--line); display: flex; align-items: center; gap: 16px; }
  .logo { width: 44px; height: 44px; flex: 0 0 auto; }
  .head h1 { margin: 0; font-size: 20px; letter-spacing: .2px; }
  .head p { margin: 2px 0 0; color: var(--muted); font-size: 13px; }
  .body { padding: 24px 32px 8px; }
  .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px 20px; }
  .kv { padding: 12px 14px; background: #0e131d; border: 1px solid var(--line); border-radius: 10px; }
  .kv .k { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: .08em; }
  .kv .v { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 14px; word-break: break-all; margin-top: 3px; }
  .hits { grid-column: 1 / -1; display: flex; align-items: baseline; justify-content: space-between; }
  .hits .n { font-size: 30px; font-weight: 700; color: var(--accent); font-family: ui-monospace, monospace; }
  .foot { padding: 16px 32px 24px; color: var(--muted); font-size: 12px; display: flex; justify-content: space-between; gap: 12px; }
  .pill { color: var(--accent2); border: 1px solid rgba(61,220,151,.35); border-radius: 999px; padding: 2px 10px; font-size: 11px; }
</style>
</head>
<body>
  <div class="card">
    <div class="head">
      <svg class="logo" viewBox="0 0 64 64" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
        <g stroke="#f5b301" stroke-width="3.5" stroke-linecap="round">
          <path d="M32 58 V20"/><path d="M32 30 L18 8"/><path d="M32 26 L46 6"/>
          <path d="M32 38 L14 22"/><path d="M32 34 L50 20"/>
        </g>
      </svg>
      <div>
        <h1>{{.Title}}</h1>
        <p>Served by a pod on your Spinifex-managed Kubernetes cluster</p>
      </div>
    </div>
    <div class="body">
      <div class="grid">
        <div class="kv"><div class="k">Pod</div><div class="v">{{.Pod}}</div></div>
        <div class="kv"><div class="k">Node</div><div class="v">{{.Node}}</div></div>
        <div class="kv"><div class="k">Cluster</div><div class="v">{{.Cluster}}</div></div>
        <div class="kv"><div class="k">Region</div><div class="v">{{.Region}}</div></div>
        <div class="kv hits">
          <div><div class="k">Requests served</div>{{if .Persisted}}<span class="pill">persisted to EBS volume</span>{{end}}</div>
          <div class="n">{{.Hits}}</div>
        </div>
      </div>
    </div>
    <div class="foot">
      <span>Namespace: {{.Namespace}}</span>
      <span>{{.Now}}</span>
    </div>
  </div>
</body>
</html>`
