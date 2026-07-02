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
  .logo { height: 44px; width: auto; flex: 0 0 auto; }
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
      <svg class="logo" viewBox="0 0 364 463" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">
        <path d="M272.523 171.205C247.773 197.595 211.323 215.815 182.103 237.425C165.663 249.575 122.633 281.165 151.093 301.825C185.533 326.835 271.333 304.985 301.013 277.585C317.193 262.645 316.693 242.485 294.703 234.075C270.913 224.975 243.483 231.925 219.353 235.755C218.543 235.885 216.823 236.805 216.993 235.345C227.053 228.885 236.103 220.925 245.933 214.135C255.403 207.585 267.053 198.225 278.643 198.015C314.693 197.345 351.803 215.135 361.203 252.215C376.833 313.885 303.333 362.255 249.833 366.635L280.913 352.235C292.563 345.305 304.353 338.105 314.653 329.215C317.213 327.005 319.073 324.145 321.243 322.035C321.753 321.535 327.053 319.265 324.003 319.125C296.253 337.335 263.793 349.415 231.323 356.335C220.173 358.715 208.323 360.995 196.963 361.285C197.243 362.845 198.643 362.525 199.663 362.845C208.823 365.795 219.313 367.545 228.843 369.005C230.163 370.005 228.733 370.415 227.683 370.555C219.063 371.665 209.923 373.305 201.293 373.745C176.843 374.985 152.203 372.435 128.573 366.425L115.293 328.755C114.153 328.865 114.073 329.895 113.783 330.725C112.613 334.015 107.893 358.025 106.313 358.365C93.6234 354.515 82.2334 345.915 72.9034 336.575L95.0434 296.915L59.7134 316.375C54.7434 305.145 53.8334 292.435 55.4934 280.355L101.213 257.645C86.9934 256.705 72.6134 258.955 58.4734 260.425C60.8734 253.495 63.8034 246.615 67.3634 240.195C68.6634 237.855 75.5634 226.305 77.3834 226.245C93.9334 229.415 111.773 230.895 128.363 227.675L92.3534 212.825L91.0334 211.045C98.3834 204.125 105.843 197.235 114.113 191.325C129.603 195.825 145.523 199.455 161.733 199.655L130.633 179.885C134.133 177.125 137.813 174.575 141.553 172.165C144.643 170.175 153.283 164.425 156.423 164.835C168.933 169.105 181.923 172.125 195.123 173.185C189.093 166.935 177.633 162.325 171.833 156.465C171.203 155.825 170.713 155.785 171.003 154.585C186.153 145.885 203.003 137.235 216.053 125.375C229.363 113.265 243.423 93.4848 233.853 75.2048C244.893 78.7148 254.393 92.6648 254.153 104.325C253.493 136.595 219.073 165.375 197.483 185.615C230.833 171.615 268.993 142.685 268.923 102.735C268.863 70.2248 239.263 57.1948 211.283 54.2348C210.103 61.3848 207.573 68.9448 203.863 75.1748C195.623 89.0148 178.803 97.6648 170.013 112.055C164.023 121.865 162.883 135.065 150.163 138.825C144.973 140.365 138.293 140.595 132.903 140.615L144.743 134.365C158.003 124.625 157.773 107.025 165.893 93.9248C164.953 92.5948 155.413 90.0748 153.183 89.5248C143.363 87.1048 126.713 84.0248 116.883 84.6948C113.843 84.9048 100.143 87.0348 98.9634 89.3748C97.2634 92.7648 105.533 107.255 107.973 110.575C108.463 111.245 109.463 111.075 109.513 111.145C109.973 111.745 109.663 113.165 108.373 112.675C107.443 112.315 101.853 108.205 100.593 107.235C90.8934 99.7648 79.8234 88.4748 71.7234 79.3648C70.4034 76.1748 68.6334 72.6148 71.7634 69.9848C88.8634 59.4048 105.793 48.3148 123.163 38.1948C174.603 8.20482 233.143 -25.6952 282.353 29.2448C323.153 74.8048 311.503 129.615 272.493 171.195L272.523 171.205ZM114.153 67.5948C127.613 65.2648 153.013 78.6248 157.193 58.8348C157.603 56.9048 156.493 55.7948 157.583 53.9148C158.403 52.5048 163.083 49.1048 164.713 47.7848C169.903 43.6048 175.823 40.3348 180.923 36.0148C161.113 39.2048 141.163 50.5648 123.853 60.5948C120.983 62.2548 117.873 63.9948 115.153 65.8548C114.453 66.3348 113.713 65.7348 114.143 67.5848L114.153 67.5948ZM21.1634 276.415C-11.8666 312.635 -4.35661 364.395 30.6734 396.135C65.5434 427.725 120.403 438.925 166.373 436.725C207.813 434.735 249.893 421.075 290.773 434.965C306.663 440.365 320.533 450.465 332.363 462.135C330.773 454.415 326.123 446.365 321.623 439.825C280.593 380.225 200.893 402.565 140.123 390.665C107.163 384.215 72.8934 369.135 52.8234 341.225C35.4734 317.105 32.7734 288.665 41.3634 260.475C39.6134 258.725 23.0334 274.355 21.1634 276.415ZM143.743 109.535C136.373 116.045 128.573 122.235 120.403 127.775L97.1634 139.135C116.843 139.885 137.193 128.635 143.753 109.545L143.743 109.535Z" fill="#f5b301"/>
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
