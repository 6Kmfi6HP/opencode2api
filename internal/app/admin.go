package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ======================== Admin 管理页面 ========================

func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	refreshOCSession()
	fetched, err := fetchModels()
	if err == nil && len(fetched) > 0 {
		modelMu.Lock()
		modelsCache = fetched
		modelsLoaded = true
		modelMu.Unlock()
		slog.Info("free models refreshed", "count", len(fetched))
	}
	goFetched, goErr := fetchGoModels()
	if goErr == nil && len(goFetched) > 0 {
		modelMu.Lock()
		goModelsCache = goFetched
		modelMu.Unlock()
		slog.Info("go catalog refreshed", "count", len(goFetched))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"session": ocSessionID,
		"free":    len(modelsCache),
		"go":      len(goModelsCache),
	})

}

func adminConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configMu.RLock()
		cfg := AppConfig{ModelAlias: modelAliasRules, ReasoningEffortMap: reasoningEffortMap, ForceDisableThinking: forceDisableThinking, MaxTokensCap: maxTokensCap, MaxTokensCapPerModel: maxTokensCapPerModel}
		configMu.RUnlock()
		socks5Mu.RLock()
		cfg.Socks5Proxies = socks5Proxies
		cfg.ActiveSocks5 = activeSocks5
		cfg.Socks5PaidDirect = socks5PaidDirect
		cfg.UpstreamBaseURLs = upstreamBaseURLs
		socks5Mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model_alias":              cfg.ModelAlias,
			"reasoning_effort_map":     cfg.ReasoningEffortMap,
			"force_disable_thinking":   cfg.ForceDisableThinking,
			"max_tokens_cap":           cfg.MaxTokensCap,
			"max_tokens_cap_per_model": cfg.MaxTokensCapPerModel,
			"socks5_proxies":           cfg.Socks5Proxies,
			"active_socks5":            cfg.ActiveSocks5,
			"socks5_paid_direct":       cfg.Socks5PaidDirect,
			"upstream_base_urls":       cfg.UpstreamBaseURLs,
			"log_level":                getLogLevelString(),
			"log_bodies":               getLogBodies(),
		})
	case http.MethodPost:
		var payload struct {
			AppConfig
			LogLevel  *string `json:"log_level,omitempty"`
			LogBodies *bool   `json:"log_bodies,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, `{"error":"Invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if err := saveConfig(configPath, payload.AppConfig); err != nil {
			http.Error(w, `{"error":"Failed to save config"}`, http.StatusInternalServerError)
			return
		}
		applyConfig(payload.AppConfig)
		if payload.LogLevel != nil {
			setLogLevelString(*payload.LogLevel)
		}
		if payload.LogBodies != nil {
			setLogBodies(*payload.LogBodies)
		}
		if debugMode {
			slog.Info("config updated",
				"aliases", len(payload.ModelAlias),
				"effort_map", len(payload.ReasoningEffortMap),
				"force_disable", payload.ForceDisableThinking,
				"max_tokens_cap", payload.MaxTokensCap,
				"max_tokens_cap_per_model", len(payload.MaxTokensCapPerModel),
				"log_level", getLogLevelString(),
				"log_bodies", getLogBodies(),
			)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminStatsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tokenStatsMu.Lock()
		data, err := json.Marshal(tokenStats)
		tokenStatsMu.Unlock()
		if err != nil {
			http.Error(w, `{"error":"marshal error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodDelete:
		tokenStatsMu.Lock()
		tokenStats = &TokenStatsData{Models: map[string]*ModelStats{}}
		tokenStatsMu.Unlock()
		if err := saveTokenStats(); err != nil {
			slog.Error("failed to save cleared token stats", "path", getTokenStatsPath(), "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "Failed to save token stats"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func adminPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminHTML))
}

func renderLoginPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(adminLoginHTML))
	if msg != "" {
		w.Write([]byte("<script>document.addEventListener('DOMContentLoaded',function(){var m=document.getElementById('login-msg');if(m){m.textContent='" + msg + "';m.style.display='block'}})</script>"))
	}
}

const adminLoginHTML = `<!DOCTYPE html>
<html lang="zh" data-theme="dark">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>登录 - OPENCODE TO API</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;600&family=Plus+Jakarta+Sans:wght@400;500;600;700;800&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #08090d;
  --surface: rgba(15, 17, 24, 0.82);
  --surface-elevated: rgba(23, 27, 38, 0.9);
  --input-bg: rgba(10, 12, 18, 0.85);
  --border: rgba(255, 255, 255, 0.08);
  --border-hover: rgba(255, 255, 255, 0.16);
  --border-focus: #6366f1;
  --text: #f8fafc;
  --text-sec: #94a3b8;
  --text-ter: #64748b;
  --accent: #6366f1;
  --accent-hover: #4f46e5;
  --accent-glow: rgba(99, 102, 241, 0.25);
  --emerald: #10b981;
  --emerald-glow: rgba(16, 185, 129, 0.2);
  --rose: #f43f5e;
  --rose-dim: rgba(244, 63, 94, 0.12);
  --radius: 16px;
  --radius-sm: 10px;
  --font: 'Plus Jakarta Sans', system-ui, -apple-system, sans-serif;
  --mono: 'JetBrains Mono', Consolas, monospace;
  --shadow-card: 0 20px 50px -10px rgba(0, 0, 0, 0.6), 0 0 0 1px var(--border);
}
[data-theme="light"] {
  --bg: #f8fafc;
  --surface: rgba(255, 255, 255, 0.9);
  --surface-elevated: rgba(241, 245, 249, 0.95);
  --input-bg: #ffffff;
  --border: #e2e8f0;
  --border-hover: #cbd5e1;
  --border-focus: #4f46e5;
  --text: #0f172a;
  --text-sec: #475569;
  --text-ter: #94a3b8;
  --accent: #4f46e5;
  --accent-hover: #4338ca;
  --accent-glow: rgba(79, 70, 229, 0.18);
  --emerald: #059669;
  --emerald-glow: rgba(5, 150, 105, 0.15);
  --rose: #e11d48;
  --rose-dim: rgba(225, 29, 72, 0.08);
  --shadow-card: 0 20px 40px -15px rgba(15, 23, 42, 0.08), 0 0 0 1px var(--border);
}
* { margin: 0; padding: 0; box-sizing: border-box; }
body {
  font-family: var(--font);
  background: var(--bg);
  color: var(--text);
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
  overflow-x: hidden;
  position: relative;
  transition: background-color 0.3s ease, color 0.3s ease;
}
#canvas-bg {
  position: fixed;
  inset: 0;
  width: 100%;
  height: 100%;
  pointer-events: none;
  z-index: 0;
}
.ambient-glow {
  position: fixed;
  width: 600px;
  height: 600px;
  border-radius: 50%;
  filter: blur(140px);
  pointer-events: none;
  opacity: 0.45;
  z-index: 0;
}
.glow-1 { top: -150px; left: 10%; background: radial-gradient(circle, var(--accent-glow) 0%, transparent 70%); }
.glow-2 { bottom: -150px; right: 10%; background: radial-gradient(circle, var(--emerald-glow) 0%, transparent 70%); }

.login-wrapper {
  width: 100%;
  max-width: 440px;
  position: relative;
  z-index: 10;
  animation: fadeInScale 0.6s cubic-bezier(0.16, 1, 0.3, 1) forwards;
}
@keyframes fadeInScale {
  from { opacity: 0; transform: scale(0.96) translateY(12px); }
  to { opacity: 1; transform: scale(1) translateY(0); }
}

.login-card {
  background: var(--surface);
  backdrop-filter: blur(24px);
  -webkit-backdrop-filter: blur(24px);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 40px 36px 36px;
  box-shadow: var(--shadow-card);
  position: relative;
  overflow: hidden;
}
.login-card::before {
  content: '';
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  height: 2px;
  background: linear-gradient(90deg, transparent 0%, var(--accent) 50%, transparent 100%);
  opacity: 0.8;
}

.top-bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 28px;
}
.badge-status {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 4px 10px;
  background: var(--surface-elevated);
  border: 1px solid var(--border);
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 600;
  letter-spacing: 0.5px;
  color: var(--emerald);
  font-family: var(--mono);
}
.pulse-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--emerald);
  box-shadow: 0 0 0 0 var(--emerald-glow);
  animation: pulse 2s infinite;
}
@keyframes pulse {
  0% { box-shadow: 0 0 0 0 rgba(16, 185, 129, 0.7); }
  70% { box-shadow: 0 0 0 6px rgba(16, 185, 129, 0); }
  100% { box-shadow: 0 0 0 0 rgba(16, 185, 129, 0); }
}

.theme-btn {
  background: var(--surface-elevated);
  border: 1px solid var(--border);
  color: var(--text-sec);
  width: 36px;
  height: 36px;
  border-radius: var(--radius-sm);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  transition: all 0.2s cubic-bezier(0.16, 1, 0.3, 1);
}
.theme-btn:hover {
  border-color: var(--border-hover);
  color: var(--text);
  transform: translateY(-1px);
}
.theme-btn:active {
  transform: scale(0.95);
}

.brand-section {
  text-align: center;
  margin-bottom: 32px;
}
.brand-icon-box {
  width: 54px;
  height: 54px;
  margin: 0 auto 16px;
  background: linear-gradient(135deg, rgba(99, 102, 241, 0.15), rgba(16, 185, 129, 0.15));
  border: 1px solid rgba(99, 102, 241, 0.3);
  border-radius: 14px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--accent);
  box-shadow: 0 8px 24px -4px var(--accent-glow);
}
.brand-title {
  font-size: 22px;
  font-weight: 800;
  letter-spacing: -0.5px;
  color: var(--text);
  margin-bottom: 6px;
}
.brand-desc {
  font-size: 13px;
  color: var(--text-sec);
  font-weight: 400;
}

.error-banner {
  display: none;
  align-items: center;
  gap: 10px;
  padding: 12px 16px;
  background: var(--rose-dim);
  border: 1px solid rgba(244, 63, 94, 0.25);
  border-radius: var(--radius-sm);
  color: var(--rose);
  font-size: 13px;
  font-weight: 500;
  margin-bottom: 20px;
  animation: shake 0.4s ease;
}
@keyframes shake {
  0%, 100% { transform: translateX(0); }
  25% { transform: translateX(-4px); }
  75% { transform: translateX(4px); }
}

.form-group {
  margin-bottom: 22px;
}
.form-label {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-size: 12px;
  font-weight: 600;
  color: var(--text-sec);
  margin-bottom: 8px;
  text-transform: uppercase;
  letter-spacing: 0.6px;
}
.input-wrapper {
  position: relative;
  display: flex;
  align-items: center;
}
.input-icon {
  position: absolute;
  left: 14px;
  color: var(--text-ter);
  pointer-events: none;
  display: flex;
  align-items: center;
}
.form-input {
  width: 100%;
  height: 46px;
  padding: 0 44px 0 42px;
  background: var(--input-bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--mono);
  font-size: 14px;
  transition: all 0.2s ease;
}
.form-input:hover {
  border-color: var(--border-hover);
}
.form-input:focus {
  outline: none;
  border-color: var(--border-focus);
  box-shadow: 0 0 0 3px var(--accent-glow);
}
.toggle-pwd {
  position: absolute;
  right: 12px;
  background: transparent;
  border: none;
  color: var(--text-ter);
  cursor: pointer;
  display: flex;
  align-items: center;
  padding: 4px;
  border-radius: 4px;
  transition: color 0.15s;
}
.toggle-pwd:hover {
  color: var(--text);
}

.submit-btn {
  width: 100%;
  height: 46px;
  background: linear-gradient(135deg, var(--accent), var(--accent-hover));
  color: #ffffff;
  border: none;
  border-radius: var(--radius-sm);
  font-family: var(--font);
  font-size: 14px;
  font-weight: 600;
  letter-spacing: 0.2px;
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  transition: all 0.2s cubic-bezier(0.16, 1, 0.3, 1);
  box-shadow: 0 4px 16px -2px var(--accent-glow);
}
.submit-btn:hover {
  transform: translateY(-1px);
  box-shadow: 0 6px 20px -2px var(--accent-glow);
  filter: brightness(1.05);
}
.submit-btn:active {
  transform: scale(0.98);
}

.footer-info {
  text-align: center;
  margin-top: 24px;
  font-size: 12px;
  color: var(--text-ter);
}
</style>
</head>
<body>
<canvas id="canvas-bg"></canvas>
<div class="ambient-glow glow-1"></div>
<div class="ambient-glow glow-2"></div>

<div class="login-wrapper">
  <div class="login-card">
    <div class="top-bar">
      <div class="badge-status">
        <span class="pulse-dot"></span>
        <span>GATEWAY READY</span>
      </div>
      <button class="theme-btn" id="themeToggle" onclick="toggleTheme()" title="切换明暗主题" aria-label="切换明暗主题">
        <svg id="themeIconSun" xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="5"></circle><line x1="12" y1="1" x2="12" y2="3"></line><line x1="12" y1="21" x2="12" y2="23"></line><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"></line><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"></line><line x1="1" y1="12" x2="3" y2="12"></line><line x1="21" y1="12" x2="23" y2="12"></line><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"></line><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"></line></svg>
        <svg id="themeIconMoon" style="display:none" xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"></path></svg>
      </button>
    </div>

    <div class="brand-section">
      <div class="brand-icon-box">
        <svg xmlns="http://www.w3.org/2000/svg" width="26" height="26" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 17 10 11 4 5"></polyline><line x1="12" y1="19" x2="20" y2="19"></line></svg>
      </div>
      <h1 class="brand-title">OPENCODE TO API</h1>
      <p class="brand-desc">请输入管理密码以进入控制面板</p>
    </div>

    <div class="error-banner" id="login-msg">
      <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line></svg>
      <span id="msg-text">密码错误</span>
    </div>

    <form method="post" action="/login">
      <div class="form-group">
        <label class="form-label" for="pwd">
          <span>安全凭证</span>
          <span>PASSWORD</span>
        </label>
        <div class="input-wrapper">
          <span class="input-icon">
            <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="11" width="18" height="11" rx="2" ry="2"></rect><path d="M7 11V7a5 5 0 0 1 10 0v4"></path></svg>
          </span>
          <input id="pwd" name="password" type="password" class="form-input" placeholder="输入控制面板密码" autocomplete="current-password" required autofocus>
          <button type="button" class="toggle-pwd" onclick="togglePasswordVisibility()" aria-label="显示/隐藏密码">
            <svg id="eyeIcon" xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"></path><circle cx="12" cy="12" r="3"></circle></svg>
          </button>
        </div>
      </div>
      <button class="submit-btn" type="submit">
        <span>进入控制台</span>
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="5" y1="12" x2="19" y2="12"></line><polyline points="12 5 19 12 12 19"></polyline></svg>
      </button>
    </form>

    <div class="footer-info">
      OPENCODE API GATEWAY - HIGH PERFORMANCE PROXY
    </div>
  </div>
</div>

<script>
(function(){
  var t = localStorage.getItem('theme');
  if(!t) t = window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  applyTheme(t);
})();

function applyTheme(theme) {
  if (theme === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
    document.getElementById('themeIconSun').style.display = 'none';
    document.getElementById('themeIconMoon').style.display = 'block';
  } else {
    document.documentElement.setAttribute('data-theme', 'dark');
    document.getElementById('themeIconSun').style.display = 'block';
    document.getElementById('themeIconMoon').style.display = 'none';
  }
}

function toggleTheme() {
  var cur = document.documentElement.getAttribute('data-theme') || 'dark';
  var next = cur === 'dark' ? 'light' : 'dark';
  localStorage.setItem('theme', next);
  applyTheme(next);
}

function togglePasswordVisibility() {
  var inp = document.getElementById('pwd');
  var eye = document.getElementById('eyeIcon');
  if (inp.type === 'password') {
    inp.type = 'text';
    eye.innerHTML = '<path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"></path><line x1="1" y1="1" x2="23" y2="23"></line>';
  } else {
    inp.type = 'password';
    eye.innerHTML = '<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"></path><circle cx="12" cy="12" r="3"></circle>';
  }
}

// Particle canvas animation
(function() {
  var canvas = document.getElementById('canvas-bg');
  if (!canvas) return;
  var ctx = canvas.getContext('2d');
  var width, height;
  var particles = [];
  var count = 38;

  function resize() {
    width = canvas.width = window.innerWidth;
    height = canvas.height = window.innerHeight;
  }
  window.addEventListener('resize', resize);
  resize();

  for (var i = 0; i < count; i++) {
    particles.push({
      x: Math.random() * width,
      y: Math.random() * height,
      vx: (Math.random() - 0.5) * 0.4,
      vy: (Math.random() - 0.5) * 0.4,
      radius: Math.random() * 1.5 + 0.8
    });
  }

  function render() {
    ctx.clearRect(0, 0, width, height);
    var isDark = document.documentElement.getAttribute('data-theme') !== 'light';
    var pColor = isDark ? 'rgba(99, 102, 241, 0.4)' : 'rgba(99, 102, 241, 0.25)';
    var lColor = isDark ? 'rgba(99, 102, 241, 0.06)' : 'rgba(99, 102, 241, 0.04)';

    for (var i = 0; i < particles.length; i++) {
      var p = particles[i];
      p.x += p.vx;
      p.y += p.vy;
      if (p.x < 0) p.x = width;
      if (p.x > width) p.x = 0;
      if (p.y < 0) p.y = height;
      if (p.y > height) p.y = 0;

      ctx.beginPath();
      ctx.arc(p.x, p.y, p.radius, 0, Math.PI * 2);
      ctx.fillStyle = pColor;
      ctx.fill();

      for (var j = i + 1; j < particles.length; j++) {
        var p2 = particles[j];
        var dx = p.x - p2.x;
        var dy = p.y - p2.y;
        var dist = Math.sqrt(dx * dx + dy * dy);
        if (dist < 130) {
          ctx.beginPath();
          ctx.moveTo(p.x, p.y);
          ctx.lineTo(p2.x, p2.y);
          ctx.strokeStyle = lColor;
          ctx.lineWidth = 1;
          ctx.stroke();
        }
      }
    }
    requestAnimationFrame(render);
  }
  render();
})();
</script>
</body>
</html>`

const adminHTML = `<!DOCTYPE html>
<html lang="zh" data-theme="dark">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>OPENCODE TO API - 控制面板</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:ital,wght@0,400;0,500;0,600;1,400&family=Plus+Jakarta+Sans:wght@400;500;600;700;800&display=swap" rel="stylesheet">
<style>
:root {
  --bg: #08090d;
  --bg-dots: rgba(255, 255, 255, 0.04);
  --surface: rgba(15, 17, 24, 0.82);
  --surface-hover: rgba(20, 23, 34, 0.9);
  --surface-elevated: rgba(24, 28, 40, 0.95);
  --surface-inset: rgba(10, 12, 18, 0.75);
  --border: rgba(255, 255, 255, 0.08);
  --border-hover: rgba(255, 255, 255, 0.16);
  --border-focus: #6366f1;
  --text: #f8fafc;
  --text-sec: #94a3b8;
  --text-ter: #64748b;
  --accent: #6366f1;
  --accent-hover: #4f46e5;
  --accent-dim: rgba(99, 102, 241, 0.14);
  --accent-glow: rgba(99, 102, 241, 0.25);
  --emerald: #10b981;
  --emerald-dim: rgba(16, 185, 129, 0.14);
  --emerald-glow: rgba(16, 185, 129, 0.2);
  --cyan: #06b6d4;
  --cyan-dim: rgba(6, 182, 212, 0.14);
  --amber: #f59e0b;
  --amber-dim: rgba(245, 158, 11, 0.14);
  --rose: #f43f5e;
  --rose-dim: rgba(244, 63, 94, 0.14);
  --radius-xl: 18px;
  --radius: 14px;
  --radius-sm: 8px;
  --radius-xs: 6px;
  --font: 'Plus Jakarta Sans', system-ui, -apple-system, sans-serif;
  --mono: 'JetBrains Mono', Consolas, monospace;
  --shadow-card: 0 16px 40px -10px rgba(0, 0, 0, 0.5), 0 0 0 1px var(--border);
  --shadow-glow: 0 0 24px -4px var(--accent-glow);
}
[data-theme="light"] {
  --bg: #f8fafc;
  --bg-dots: rgba(15, 23, 42, 0.04);
  --surface: rgba(255, 255, 255, 0.92);
  --surface-hover: #ffffff;
  --surface-elevated: #f1f5f9;
  --surface-inset: #f8fafc;
  --border: #e2e8f0;
  --border-hover: #cbd5e1;
  --border-focus: #4f46e5;
  --text: #0f172a;
  --text-sec: #475569;
  --text-ter: #94a3b8;
  --accent: #4f46e5;
  --accent-hover: #4338ca;
  --accent-dim: rgba(79, 70, 229, 0.08);
  --accent-glow: rgba(79, 70, 229, 0.18);
  --emerald: #059669;
  --emerald-dim: rgba(5, 150, 105, 0.08);
  --emerald-glow: rgba(5, 150, 105, 0.15);
  --cyan: #0891b2;
  --cyan-dim: rgba(8, 145, 178, 0.08);
  --amber: #d97706;
  --amber-dim: rgba(217, 119, 6, 0.08);
  --rose: #e11d48;
  --rose-dim: rgba(225, 29, 72, 0.08);
  --shadow-card: 0 12px 30px -10px rgba(15, 23, 42, 0.06), 0 0 0 1px var(--border);
  --shadow-glow: 0 0 20px -4px var(--accent-glow);
}
* { margin: 0; padding: 0; box-sizing: border-box; }
body {
  font-family: var(--font);
  background: var(--bg);
  color: var(--text);
  font-size: 13.5px;
  line-height: 1.55;
  min-height: 100vh;
  position: relative;
  overflow-x: hidden;
  transition: background-color 0.3s ease, color 0.3s ease;
}
#ambient-canvas {
  position: fixed;
  inset: 0;
  width: 100%;
  height: 100%;
  pointer-events: none;
  z-index: 0;
}
.ambient-glow-1 {
  position: fixed;
  top: -180px;
  left: 15%;
  width: 500px;
  height: 500px;
  border-radius: 50%;
  background: radial-gradient(circle, var(--accent-glow) 0%, transparent 70%);
  filter: blur(120px);
  pointer-events: none;
  z-index: 0;
  opacity: 0.5;
}
.ambient-glow-2 {
  position: fixed;
  top: 400px;
  right: 5%;
  width: 450px;
  height: 450px;
  border-radius: 50%;
  background: radial-gradient(circle, var(--emerald-glow) 0%, transparent 70%);
  filter: blur(120px);
  pointer-events: none;
  z-index: 0;
  opacity: 0.4;
}

.container {
  max-width: 1180px;
  margin: 0 auto;
  padding: 28px 24px 80px;
  position: relative;
  z-index: 10;
}

/* Glass Header */
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 22px;
  background: var(--surface);
  backdrop-filter: blur(20px);
  -webkit-backdrop-filter: blur(20px);
  border: 1px solid var(--border);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-card);
  margin-bottom: 24px;
}
.brand-group {
  display: flex;
  align-items: center;
  gap: 14px;
}
.brand-logo {
  width: 42px;
  height: 42px;
  background: linear-gradient(135deg, rgba(99, 102, 241, 0.2), rgba(16, 185, 129, 0.2));
  border: 1px solid rgba(99, 102, 241, 0.35);
  border-radius: 12px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--accent);
  box-shadow: 0 4px 16px -2px var(--accent-glow);
  flex-shrink: 0;
}
.brand-info {
  display: flex;
  flex-direction: column;
}
.brand-title-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.brand-title {
  font-size: 18px;
  font-weight: 800;
  letter-spacing: -0.4px;
  color: var(--text);
}
.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 2px 8px;
  background: var(--emerald-dim);
  border: 1px solid rgba(16, 185, 129, 0.25);
  border-radius: 999px;
  font-size: 11px;
  font-weight: 600;
  font-family: var(--mono);
  color: var(--emerald);
  letter-spacing: 0.3px;
}
.pulse-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--emerald);
  box-shadow: 0 0 0 0 rgba(16, 185, 129, 0.6);
  animation: pulseRadar 2s infinite;
}
@keyframes pulseRadar {
  0% { box-shadow: 0 0 0 0 rgba(16, 185, 129, 0.7); }
  70% { box-shadow: 0 0 0 5px rgba(16, 185, 129, 0); }
  100% { box-shadow: 0 0 0 0 rgba(16, 185, 129, 0); }
}
.brand-subtitle {
  font-size: 12px;
  color: var(--text-sec);
  font-weight: 400;
  margin-top: 1px;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

/* Glass Card */
.glass-card {
  background: var(--surface);
  backdrop-filter: blur(20px);
  -webkit-backdrop-filter: blur(20px);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 22px;
  box-shadow: var(--shadow-card);
  position: relative;
  overflow: hidden;
  transition: border-color 0.2s cubic-bezier(0.16, 1, 0.3, 1), transform 0.2s ease;
}
.glass-card:hover {
  border-color: var(--border-hover);
}

/* Hero HUD Grid */
.hero-hud {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 16px;
  margin-bottom: 20px;
}
.hud-tile {
  background: var(--surface);
  backdrop-filter: blur(20px);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 18px 20px;
  position: relative;
  overflow: hidden;
  box-shadow: var(--shadow-card);
  transition: all 0.25s cubic-bezier(0.16, 1, 0.3, 1);
}
.hud-tile:hover {
  border-color: var(--border-hover);
  transform: translateY(-2px);
}
.hud-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 10px;
}
.hud-title {
  font-size: 11.5px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.6px;
  color: var(--text-ter);
}
.hud-icon-box {
  width: 32px;
  height: 32px;
  border-radius: 8px;
  display: flex;
  align-items: center;
  justify-content: center;
}
.icon-emerald { background: var(--emerald-dim); color: var(--emerald); }
.icon-cyan { background: var(--cyan-dim); color: var(--cyan); }
.icon-indigo { background: var(--accent-dim); color: var(--accent); }
.icon-amber { background: var(--amber-dim); color: var(--amber); }

.hud-value {
  font-size: 24px;
  font-weight: 800;
  font-family: var(--mono);
  color: var(--text);
  letter-spacing: -0.5px;
  line-height: 1.2;
}
.hud-subtext {
  font-size: 12px;
  color: var(--text-sec);
  margin-top: 6px;
  display: flex;
  align-items: center;
  gap: 6px;
}
.hud-bar {
  height: 4px;
  width: 100%;
  background: var(--surface-elevated);
  border-radius: 999px;
  margin-top: 8px;
  overflow: hidden;
  display: flex;
}
.hud-bar-prompt { height: 100%; background: var(--accent); }
.hud-bar-comp { height: 100%; background: var(--emerald); }

/* Main Bento Grid */
.bento-grid {
  display: grid;
  grid-template-columns: repeat(12, 1fr);
  gap: 20px;
}
.col-12 { grid-column: span 12; }
.col-8 { grid-column: span 8; }
.col-7 { grid-column: span 7; }
.col-6 { grid-column: span 6; }
.col-5 { grid-column: span 5; }
.col-4 { grid-column: span 4; }

.section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 18px;
  flex-wrap: wrap;
  gap: 12px;
}
.section-title {
  font-size: 14px;
  font-weight: 700;
  letter-spacing: -0.2px;
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--text);
}
.section-title svg {
  color: var(--accent);
}
.section-badge {
  font-size: 11px;
  font-weight: 600;
  font-family: var(--mono);
  padding: 2px 7px;
  border-radius: 999px;
  background: var(--surface-elevated);
  color: var(--text-ter);
  border: 1px solid var(--border);
}

/* Buttons */
.btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 7px 14px;
  font-size: 12.5px;
  font-weight: 600;
  font-family: var(--font);
  border-radius: var(--radius-sm);
  border: 1px solid transparent;
  cursor: pointer;
  white-space: nowrap;
  transition: all 0.2s cubic-bezier(0.16, 1, 0.3, 1);
  text-decoration: none;
}
.btn:active {
  transform: scale(0.97);
}
.btn-primary {
  background: var(--accent);
  color: #ffffff;
  box-shadow: 0 4px 14px -2px var(--accent-glow);
}
.btn-primary:hover {
  background: var(--accent-hover);
  box-shadow: 0 6px 18px -2px var(--accent-glow);
}
.btn-secondary {
  background: var(--surface-elevated);
  border-color: var(--border);
  color: var(--text);
}
.btn-secondary:hover {
  border-color: var(--border-hover);
  background: var(--surface-hover);
}
.btn-emerald {
  background: var(--emerald-dim);
  border-color: rgba(16, 185, 129, 0.3);
  color: var(--emerald);
}
.btn-emerald:hover {
  background: var(--emerald);
  color: #ffffff;
  box-shadow: 0 4px 14px -2px var(--emerald-glow);
}
.btn-rose {
  background: var(--rose-dim);
  border-color: rgba(244, 63, 94, 0.25);
  color: var(--rose);
}
.btn-rose:hover {
  background: var(--rose);
  color: #ffffff;
}
.btn-icon {
  width: 36px;
  height: 36px;
  padding: 0;
  border-radius: var(--radius-sm);
}

/* Search Bar */
.search-box {
  display: flex;
  align-items: center;
  gap: 8px;
  background: var(--surface-inset);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 4px 10px;
  transition: border-color 0.15s;
}
.search-box:focus-within {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-dim);
}
.search-box input {
  background: transparent;
  border: none;
  outline: none;
  font-family: var(--font);
  font-size: 12px;
  color: var(--text);
  width: 140px;
}
.search-box input::placeholder {
  color: var(--text-ter);
}

/* Modern Tables */
.table-wrapper {
  width: 100%;
  overflow-x: auto;
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: var(--surface-inset);
}
.modern-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 12.5px;
  text-align: left;
}
.modern-table th {
  padding: 10px 14px;
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: var(--text-ter);
  background: var(--surface-elevated);
  border-bottom: 1px solid var(--border);
  white-space: nowrap;
}
.modern-table td {
  padding: 10px 14px;
  border-bottom: 1px solid var(--border);
  color: var(--text);
  vertical-align: middle;
}
.modern-table tbody tr {
  transition: background-color 0.15s;
}
.modern-table tbody tr:hover {
  background: var(--surface-hover);
}
.modern-table tbody tr:last-child td {
  border-bottom: none;
}
.modern-table .num-cell {
  font-family: var(--mono);
  color: var(--text-sec);
}
.modern-table .total-row td {
  font-weight: 700;
  color: var(--text);
  background: var(--surface-elevated);
  border-top: 1px solid var(--border-hover);
}

/* Input Fields inside Tables & Forms */
.form-control {
  width: 100%;
  padding: 7px 11px;
  background: var(--surface-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-xs);
  color: var(--text);
  font-family: var(--mono);
  font-size: 12.5px;
  transition: all 0.15s ease;
}
.form-control:hover {
  border-color: var(--border-hover);
}
.form-control:focus {
  outline: none;
  border-color: var(--border-focus);
  box-shadow: 0 0 0 2px var(--accent-dim);
}
.form-select {
  appearance: none;
  -webkit-appearance: none;
  background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='%2394a3b8' stroke-width='2' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpolyline points='6 9 12 15 18 9'%3E%3C/polyline%3E%3C/svg%3E");
  background-repeat: no-repeat;
  background-position: right 10px center;
  padding-right: 28px;
}
textarea.form-control {
  line-height: 1.5;
  resize: vertical;
}

/* Tactical Switch / Checkbox */
.switch-wrapper {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 14px;
  background: var(--surface-inset);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  margin-top: 12px;
  margin-bottom: 14px;
  transition: border-color 0.15s;
}
.switch-wrapper:hover {
  border-color: var(--border-hover);
}
.switch-label-group {
  display: flex;
  flex-direction: column;
}
.switch-title {
  font-size: 12.5px;
  font-weight: 600;
  color: var(--text);
}
.switch-desc {
  font-size: 11.5px;
  color: var(--text-ter);
  margin-top: 2px;
}
.custom-switch {
  position: relative;
  display: inline-block;
  width: 40px;
  height: 22px;
  flex-shrink: 0;
}
.custom-switch input {
  opacity: 0;
  width: 0;
  height: 0;
}
.switch-track {
  position: absolute;
  cursor: pointer;
  inset: 0;
  background: var(--surface-elevated);
  border: 1px solid var(--border);
  border-radius: 999px;
  transition: 0.25s cubic-bezier(0.16, 1, 0.3, 1);
}
.switch-track::before {
  position: absolute;
  content: "";
  height: 16px;
  width: 16px;
  left: 2px;
  bottom: 2px;
  background-color: var(--text-ter);
  border-radius: 50%;
  transition: 0.25s cubic-bezier(0.16, 1, 0.3, 1);
}
.custom-switch input:checked + .switch-track {
  background-color: var(--accent);
  border-color: var(--accent);
}
.custom-switch input:checked + .switch-track::before {
  transform: translateX(18px);
  background-color: #ffffff;
}

.empty-state {
  padding: 32px 16px;
  text-align: center;
  color: var(--text-ter);
  font-size: 12.5px;
}

.card-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 14px;
  flex-wrap: wrap;
}

/* Floating Save Action Bar */
.floating-dock {
  position: fixed;
  bottom: 24px;
  right: 24px;
  z-index: 990;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 8px 10px 8px 16px;
  background: var(--surface);
  backdrop-filter: blur(24px);
  -webkit-backdrop-filter: blur(24px);
  border: 1px solid var(--border);
  border-radius: 999px;
  box-shadow: 0 16px 36px -6px rgba(0, 0, 0, 0.5), 0 0 0 1px var(--border);
  animation: slideUp 0.4s cubic-bezier(0.16, 1, 0.3, 1) forwards;
}
@keyframes slideUp {
  from { transform: translateY(20px); opacity: 0; }
  to { transform: translateY(0); opacity: 1; }
}
.dock-hint {
  font-size: 12px;
  font-family: var(--mono);
  color: var(--text-ter);
}
.dock-save-btn {
  background: linear-gradient(135deg, var(--accent), var(--accent-hover));
  color: #ffffff;
  border: none;
  padding: 8px 18px;
  border-radius: 999px;
  font-size: 13px;
  font-weight: 700;
  cursor: pointer;
  display: flex;
  align-items: center;
  gap: 7px;
  box-shadow: 0 4px 16px -2px var(--accent-glow);
  transition: all 0.2s cubic-bezier(0.16, 1, 0.3, 1);
}
.dock-save-btn:hover {
  transform: translateY(-1px);
  box-shadow: 0 6px 20px -2px var(--accent-glow);
}
.dock-save-btn:active {
  transform: scale(0.97);
}

/* Toast */
#toast {
  position: fixed;
  top: 24px;
  right: 24px;
  padding: 12px 18px;
  border-radius: var(--radius);
  font-size: 13px;
  font-weight: 600;
  display: flex;
  align-items: center;
  gap: 10px;
  opacity: 0;
  transform: translateY(-12px) scale(0.95);
  transition: all 0.25s cubic-bezier(0.16, 1, 0.3, 1);
  z-index: 1000;
  pointer-events: none;
  box-shadow: 0 12px 30px -4px rgba(0, 0, 0, 0.4);
}
#toast.show {
  opacity: 1;
  transform: translateY(0) scale(1);
}
#toast.success {
  background: rgba(16, 185, 129, 0.95);
  color: #ffffff;
  backdrop-filter: blur(12px);
}
#toast.error {
  background: rgba(244, 63, 94, 0.95);
  color: #ffffff;
  backdrop-filter: blur(12px);
}

/* Modal */
.modal-backdrop {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.65);
  backdrop-filter: blur(8px);
  z-index: 1050;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 20px;
  opacity: 0;
  pointer-events: none;
  transition: opacity 0.2s ease;
}
.modal-backdrop.show {
  opacity: 1;
  pointer-events: auto;
}
.modal-card {
  width: 100%;
  max-width: 400px;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 24px;
  box-shadow: var(--shadow-card);
  transform: scale(0.95);
  transition: transform 0.2s cubic-bezier(0.16, 1, 0.3, 1);
}
.modal-backdrop.show .modal-card {
  transform: scale(1);
}
.modal-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.modal-icon-danger {
  color: var(--rose);
}
.modal-title {
  font-size: 16px;
  font-weight: 700;
  color: var(--text);
}
.modal-body {
  font-size: 13px;
  color: var(--text-sec);
  line-height: 1.5;
  margin-bottom: 20px;
}
.modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}

@media (max-width: 900px) {
  .hero-hud { grid-template-columns: repeat(2, 1fr); }
  .col-8, .col-7, .col-6, .col-5, .col-4 { grid-column: span 12; }
}
@media (max-width: 600px) {
  .hero-hud { grid-template-columns: 1fr; }
  .container { padding: 16px 12px 90px; }
  header { flex-direction: column; align-items: stretch; gap: 14px; }
  .header-actions { justify-content: flex-end; }
}
</style>
</head>
<body>
<canvas id="ambient-canvas"></canvas>
<div class="ambient-glow-1"></div>
<div class="ambient-glow-2"></div>

<div class="container">
  <!-- Header HUD -->
  <header>
    <div class="brand-group">
      <div class="brand-logo">
        <svg xmlns="http://www.w3.org/2000/svg" width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="4 17 10 11 4 5"></polyline><line x1="12" y1="19" x2="20" y2="19"></line></svg>
      </div>
      <div class="brand-info">
        <div class="brand-title-row">
          <span class="brand-title">OPENCODE TO API</span>
          <span class="status-pill">
            <span class="pulse-dot"></span>
            <span>GATEWAY ONLINE</span>
          </span>
        </div>
        <span class="brand-subtitle">OpenCode 免费 API 模型代理网关</span>
      </div>
    </div>
    <div class="header-actions">
      <button class="btn btn-emerald" onclick="reloadConfig()" id="btnReloadSession" title="重新获取上游会话与模型列表">
        <svg id="reloadIcon" xmlns="http://www.w3.org/2000/svg" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="23 4 23 10 17 10"></polyline><polyline points="1 20 1 14 7 14"></polyline><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"></path></svg>
        <span>刷新会话</span>
      </button>
      <button class="btn btn-secondary btn-icon" onclick="toggleTheme()" title="切换明暗主题" aria-label="切换主题">
        <svg id="themeIconSun" xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="5"></circle><line x1="12" y1="1" x2="12" y2="3"></line><line x1="12" y1="21" x2="12" y2="23"></line><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"></line><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"></line><line x1="1" y1="12" x2="3" y2="12"></line><line x1="21" y1="12" x2="23" y2="12"></line><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"></line><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"></line></svg>
        <svg id="themeIconMoon" style="display:none" xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"></path></svg>
      </button>
      <form method="post" action="/logout" style="margin:0">
        <button class="btn btn-secondary" type="submit" title="安全退出登录">
          <svg xmlns="http://www.w3.org/2000/svg" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"></path><polyline points="16 17 21 12 16 7"></polyline><line x1="21" y1="12" x2="9" y2="12"></line></svg>
          <span>退出</span>
        </button>
      </form>
    </div>
  </header>

  <!-- Hero HUD 4 Tiles -->
  <div class="hero-hud">
    <div class="hud-tile">
      <div class="hud-header">
        <span class="hud-title">总请求次数</span>
        <div class="hud-icon-box icon-emerald">
          <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"></polyline></svg>
        </div>
      </div>
      <div class="hud-value" id="hudTotalRequests">0</div>
      <div class="hud-subtext" id="hudActiveModels">活跃模型: 0</div>
    </div>

    <div class="hud-tile">
      <div class="hud-header">
        <span class="hud-title">总 Token 吞吐</span>
        <div class="hud-icon-box icon-cyan">
          <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="13 2 3 14 12 14 11 22 21 10 12 10 13 2"></polygon></svg>
        </div>
      </div>
      <div class="hud-value" id="hudTotalTokens">0</div>
      <div class="hud-subtext" id="hudPromptCompletionSum">输入 + 输出总计</div>
    </div>

    <div class="hud-tile">
      <div class="hud-header">
        <span class="hud-title">输入 / 输出 分布</span>
        <div class="hud-icon-box icon-indigo">
          <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="12 2 2 7 12 12 22 7 12 2"></polygon><polyline points="2 17 12 22 22 17"></polyline><polyline points="2 12 12 17 22 12"></polyline></svg>
        </div>
      </div>
      <div class="hud-value" id="hudRatioText">0% / 0%</div>
      <div class="hud-bar">
        <div class="hud-bar-prompt" id="hudBarPrompt" style="width:50%"></div>
        <div class="hud-bar-comp" id="hudBarComp" style="width:50%"></div>
      </div>
      <div class="hud-subtext" style="justify-content:space-between">
        <span id="hudPromptTokensText">入: 0</span>
        <span id="hudCompTokensText">出: 0</span>
      </div>
    </div>

    <div class="hud-tile">
      <div class="hud-header">
        <span class="hud-title">缓存吞吐</span>
        <div class="hud-icon-box icon-amber">
          <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="5" rx="9" ry="3"></ellipse><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"></path><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"></path></svg>
        </div>
      </div>
      <div class="hud-value" id="hudCacheRead">0</div>
      <div class="hud-subtext" id="hudCacheWrite">写入: 0</div>
    </div>
  </div>

  <!-- Main Bento Content -->
  <div class="bento-grid">
    <!-- Token Statistics Section (Full Width) -->
    <div class="glass-card col-12">
      <div class="section-head">
        <div class="section-title">
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12"></polyline></svg>
          <span>Token 详细统计</span>
          <span class="section-badge" id="statsModelCount">0 MODELS</span>
        </div>
        <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
          <div class="search-box">
            <svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="8"></circle><line x1="21" y1="21" x2="16.65" y2="16.65"></line></svg>
            <input type="text" id="statsFilterInput" placeholder="过滤模型名称..." oninput="filterStatsTable()">
          </div>
          <button class="btn btn-secondary" onclick="loadStats()" title="即时刷新统计">
            <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="23 4 23 10 17 10"></polyline><polyline points="1 20 1 14 7 14"></polyline><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"></path></svg>
            <span>刷新</span>
          </button>
          <button class="btn btn-rose" onclick="openResetModal()" title="清空全部 Token 历史">
            <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg>
            <span>清空统计</span>
          </button>
        </div>
      </div>
      <div class="table-wrapper">
        <table class="modern-table" id="statsTable">
          <thead>
            <tr>
              <th>模型名称</th>
              <th>请求次数</th>
              <th>输入 Token</th>
              <th>输出 Token</th>
              <th>总计 Token</th>
              <th>缓存读取</th>
              <th>缓存写入</th>
            </tr>
          </thead>
          <tbody id="statsTableBody">
            <tr><td colspan="7" class="empty-state">加载中...</td></tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Reasoning Effort Mapping Section -->
    <div class="glass-card col-6">
      <div class="section-head">
        <div class="section-title">
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="4" y1="21" x2="4" y2="14"></line><line x1="4" y1="10" x2="4" y2="3"></line><line x1="12" y1="21" x2="12" y2="12"></line><line x1="12" y1="8" x2="12" y2="3"></line><line x1="20" y1="21" x2="20" y2="16"></line><line x1="20" y1="12" x2="20" y2="3"></line><line x1="1" y1="14" x2="7" y2="14"></line><line x1="9" y1="8" x2="15" y2="8"></line><line x1="17" y1="16" x2="23" y2="16"></line></svg>
          <span>推理力度映射</span>
        </div>
      </div>
      <div class="table-wrapper" style="margin-bottom:12px">
        <table class="modern-table" id="effortTable">
          <thead>
            <tr>
              <th style="width:45%">请求值 (e.g. xhigh)</th>
              <th style="width:45%">映射值 (e.g. max)</th>
              <th style="width:10%;text-align:center">操作</th>
            </tr>
          </thead>
          <tbody></tbody>
        </table>
      </div>
      <div class="switch-wrapper">
        <div class="switch-label-group">
          <span class="switch-title">强制禁用思考模式</span>
          <span class="switch-desc">过滤并移除所有模型返回的推理内容</span>
        </div>
        <label class="custom-switch">
          <input type="checkbox" id="force_disable_thinking">
          <span class="switch-track"></span>
        </label>
      </div>
      <div class="card-actions">
        <button class="btn btn-secondary" onclick="addEffortRow()">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"></line><line x1="5" y1="12" x2="19" y2="12"></line></svg>
          <span>添加映射</span>
        </button>
      </div>
    </div>

    <!-- max_tokens Limits Section -->
    <div class="glass-card col-6">
      <div class="section-head">
        <div class="section-title">
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2v4M4.93 4.93l2.83 2.83M2 12h4M4.93 19.07l2.83-2.83M12 22v-4M19.07 19.07l-2.83-2.83M22 12h-4M19.07 4.93l-2.83 2.83"/><circle cx="12" cy="12" r="2"/></svg>
          <span>max_tokens 上限控制</span>
        </div>
      </div>
      <div style="margin-bottom:14px">
        <label style="display:block;font-size:11.5px;font-weight:600;color:var(--text-ter);margin-bottom:6px;text-transform:uppercase;letter-spacing:0.5px">全局默认上限</label>
        <div style="display:flex;align-items:center;gap:10px">
          <input type="number" id="maxTokensCap" class="form-control" min="0" placeholder="0 表示不限制" style="max-width:180px">
          <span style="font-size:12px;color:var(--text-ter)">超过此值的请求会被截断至该数值</span>
        </div>
      </div>
      <div class="table-wrapper" style="margin-bottom:12px">
        <table class="modern-table" id="capTable">
          <thead>
            <tr>
              <th style="width:55%">目标模型</th>
              <th style="width:35%">上限值</th>
              <th style="width:10%;text-align:center">操作</th>
            </tr>
          </thead>
          <tbody></tbody>
        </table>
      </div>
      <div class="card-actions">
        <button class="btn btn-secondary" onclick="addCapRow()">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"></line><line x1="5" y1="12" x2="19" y2="12"></line></svg>
          <span>添加模型上限</span>
        </button>
      </div>
    </div>

    <!-- Model Keyword Rules Section -->
    <div class="glass-card col-12">
      <div class="section-head" style="flex-wrap:wrap;gap:10px">
        <div class="section-title">
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7V4h16v3M9 20h6M12 4v16"/></svg>
          <span>模型关键词映射规则 (Keyword Rules)</span>
        </div>
        <div style="display:flex;align-items:center;gap:6px;flex-wrap:wrap">
          <span style="font-size:12px;color:var(--text-ter);margin-right:2px">快捷预设:</span>
          <button type="button" class="btn btn-xs btn-secondary" onclick="addPresetRule('sonnet','claude-sonnet-4.6')">+ sonnet</button>
          <button type="button" class="btn btn-xs btn-secondary" onclick="addPresetRule('opus','claude-opus-4.6')">+ opus</button>
          <button type="button" class="btn btn-xs btn-secondary" onclick="addPresetRule('haiku','claude-haiku-4.6')">+ haiku</button>
          <button type="button" class="btn btn-xs btn-secondary" onclick="addPresetRule('sol','gpt-5.6')">+ sol</button>
          <button type="button" class="btn btn-xs btn-secondary" onclick="addPresetRule('luna','deepseek-v4')">+ luna</button>
          <button type="button" class="btn btn-xs btn-primary" onclick="loadAllPresets()">⚡ 一键载入全套预设</button>
        </div>
      </div>
      <div style="font-size:12px;color:var(--text-ter);margin-bottom:12px">
        按顺序从上到下匹配客户端请求模型名。剥离方括号后缀后，若命中关键词将路由至目标上游模型，并自动保留原后缀。
      </div>
      <div class="table-wrapper" style="margin-bottom:12px">
        <table class="modern-table" id="keywordRuleTable">
          <thead>
            <tr>
              <th style="width:6%;text-align:center">启用</th>
              <th style="width:16%">匹配模式</th>
              <th style="width:28%">关键词 / 表达式</th>
              <th style="width:30%">目标模型 (Upstream)</th>
              <th style="width:10%;text-align:center">忽略大小写</th>
              <th style="width:10%;text-align:center">操作</th>
            </tr>
          </thead>
          <tbody></tbody>
        </table>
      </div>
      <div class="card-actions" style="margin-bottom:16px">
        <button type="button" class="btn btn-secondary" onclick="addKeywordRuleRow()">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"></line><line x1="5" y1="12" x2="19" y2="12"></line></svg>
          <span>添加映射规则</span>
        </button>
      </div>

      <!-- Live Match Tester -->
      <div style="background:rgba(255,255,255,0.03);border:1px solid var(--border-color);border-radius:8px;padding:14px">
        <div style="font-size:12.5px;font-weight:600;color:var(--text-sec);margin-bottom:8px;display:flex;align-items:center;gap:6px">
          <span>🔍 规则实时模拟测试 (Live Match Tester)</span>
        </div>
        <div style="display:flex;gap:12px;align-items:center;flex-wrap:wrap">
          <input type="text" id="testModelInput" class="form-control" placeholder="输入客户端模型名测试 (例如: claude-3-7-sonnet-20250219[1m] 或 sol-code)" style="flex:1;min-width:240px" oninput="simulateModelResolve()">
          <div id="testResultBox" style="font-size:12.5px;padding:8px 12px;border-radius:6px;background:rgba(0,0,0,0.2);color:var(--text-sec);min-width:280px">等待输入...</div>
        </div>
      </div>
    </div>

    <!-- SOCKS5 Proxy Cluster Section -->
    <div class="glass-card col-6">
      <div class="section-head">
        <div class="section-title">
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"></path></svg>
          <span>SOCKS5 代理集群</span>
        </div>
      </div>
      <div class="table-wrapper" style="margin-bottom:12px">
        <table class="modern-table" id="socks5Table">
          <thead>
            <tr>
              <th style="width:25%">名称</th>
              <th style="width:30%">地址 (Host:Port)</th>
              <th style="width:20%">用户名</th>
              <th style="width:20%">密码</th>
              <th style="width:5%;text-align:center">操作</th>
            </tr>
          </thead>
          <tbody></tbody>
        </table>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:11.5px;font-weight:600;color:var(--text-ter);margin-bottom:6px;text-transform:uppercase;letter-spacing:0.5px">当前生效路由</label>
        <select id="activeSocks5" class="form-control form-select"></select>
      </div>
      <div class="switch-wrapper">
        <div class="switch-label-group">
          <span class="switch-title">带 key / 付费请求直连</span>
          <span class="switch-desc">默认关闭：关闭时 public 与付费请求均走 SOCKS5 代理</span>
        </div>
        <label class="custom-switch">
          <input type="checkbox" id="socks5_paid_direct">
          <span class="switch-track"></span>
        </label>
      </div>
      <div class="card-actions">
        <button class="btn btn-secondary" onclick="addSocks5Row()">
          <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="5" x2="12" y2="19"></line><line x1="5" y1="12" x2="19" y2="12"></line></svg>
          <span>添加代理节点</span>
        </button>
      </div>
    </div>

    <!-- Upstream Domains Section -->
    <div class="glass-card col-8">
      <div class="section-head">
        <div class="section-title">
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="2" y1="12" x2="22" y2="12"></line><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"></path></svg>
          <span>上游域名负载均衡 (Upstream URLs)</span>
        </div>
      </div>
      <div style="margin-bottom:8px">
        <textarea id="upstreamBaseURLs" rows="4" class="form-control" placeholder="https://opencode.ai"></textarea>
      </div>
      <p style="font-size:12px;color:var(--text-ter)">
        每行一个上游反代域名。多个域名可实现负载均衡与健康轮询；同一会话将被 sticky 固定到固定的反代与代理组合。留空默认为 https://opencode.ai。
      </p>
    </div>

    <!-- Logging & Diagnostics Section -->
    <div class="glass-card col-4">
      <div class="section-head">
        <div class="section-title">
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="4" width="16" height="16" rx="2"/><rect x="9" y="9" width="6" height="6"/><line x1="9" y1="1" x2="9" y2="4"/><line x1="15" y1="1" x2="15" y2="4"/><line x1="9" y1="20" x2="9" y2="23"/><line x1="15" y1="20" x2="15" y2="23"/><line x1="20" y1="9" x2="23" y2="9"/><line x1="20" y1="14" x2="23" y2="14"/><line x1="1" y1="9" x2="4" y2="9"/><line x1="1" y1="14" x2="4" y2="14"/></svg>
          <span>日志与运行诊断</span>
        </div>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:11.5px;font-weight:600;color:var(--text-ter);margin-bottom:6px;text-transform:uppercase;letter-spacing:0.5px">日志级别</label>
        <select id="logLevelSelect" class="form-control form-select">
          <option value="debug">DEBUG (详细排查)</option>
          <option value="info">INFO (常规运行)</option>
          <option value="warn">WARN (仅告警)</option>
          <option value="error">ERROR (仅错误)</option>
        </select>
      </div>
      <div class="switch-wrapper" style="margin-top:0">
        <div class="switch-label-group">
          <span class="switch-title">记录请求体摘要</span>
          <span class="switch-desc">在 Debug 级别下截取并记录 body 摘要</span>
        </div>
        <label class="custom-switch">
          <input type="checkbox" id="logBodiesCheck">
          <span class="switch-track"></span>
        </label>
      </div>
    </div>
  </div>
</div>

<!-- Floating Save Action Bar -->
<div class="floating-dock">
  <span class="dock-hint">Ctrl + S 快捷保存</span>
  <button class="dock-save-btn" onclick="saveConfig()" title="保存当前全部配置">
    <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"></path><polyline points="17 21 17 13 7 13 7 21"></polyline><polyline points="7 3 7 8 15 8"></polyline></svg>
    <span>保存全部配置</span>
  </button>
</div>

<!-- Toast Notification -->
<div id="toast">
  <svg id="toastIcon" xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>
  <span id="toastMsg">配置已保存</span>
</div>

<!-- Reset Confirm Modal -->
<div class="modal-backdrop" id="resetModal">
  <div class="modal-card">
    <div class="modal-head">
      <svg class="modal-icon-danger" xmlns="http://www.w3.org/2000/svg" width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line></svg>
      <span class="modal-title">清空 Token 统计数据</span>
    </div>
    <div class="modal-body">
      您确定要清空所有模型的请求次数与 Token 统计历史吗？此操作不可撤销。
    </div>
    <div class="modal-actions">
      <button class="btn btn-secondary" onclick="closeResetModal()">取消</button>
      <button class="btn btn-rose" onclick="confirmResetStats()">确认清空</button>
    </div>
  </div>
</div>

<script>
let effortData = {}, modelList = [], socks5Data = [], capData = {};
let keywordRulesData = [];

const KEYWORD_PRESETS = [
  { keyword: 'sonnet', target: 'claude-sonnet-4.6', match_type: 'contains', case_insensitive: true, enabled: true },
  { keyword: 'opus', target: 'claude-opus-4.6', match_type: 'contains', case_insensitive: true, enabled: true },
  { keyword: 'haiku', target: 'claude-haiku-4.6', match_type: 'contains', case_insensitive: true, enabled: true },
  { keyword: 'sol', target: 'gpt-5.6', match_type: 'contains', case_insensitive: true, enabled: true },
  { keyword: 'luna', target: 'deepseek-v4', match_type: 'contains', case_insensitive: true, enabled: true }
];
let allStatsData = {};

// Theme Management
(function() {
  var t = localStorage.getItem('theme');
  if (!t) t = window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
  applyTheme(t);
})();

function applyTheme(theme) {
  if (theme === 'light') {
    document.documentElement.setAttribute('data-theme', 'light');
    document.getElementById('themeIconSun').style.display = 'none';
    document.getElementById('themeIconMoon').style.display = 'block';
  } else {
    document.documentElement.setAttribute('data-theme', 'dark');
    document.getElementById('themeIconSun').style.display = 'block';
    document.getElementById('themeIconMoon').style.display = 'none';
  }
}

function toggleTheme() {
  var cur = document.documentElement.getAttribute('data-theme') || 'dark';
  var next = cur === 'dark' ? 'light' : 'dark';
  localStorage.setItem('theme', next);
  applyTheme(next);
}

// Reload Session & Models
async function reloadConfig() {
  const icon = document.getElementById('reloadIcon');
  if (icon) icon.style.animation = 'spin 0.8s linear infinite';
  try {
    const r = await fetch('/api/reload', { method: 'POST' });
    const d = await r.json();
    showToast('会话已刷新 (免费模型 ' + (d.free || 0) + ' 个，Pro ' + (d.go || 0) + ' 个)', 'success');
  } catch (e) {
    showToast('刷新失败: ' + e.message, 'error');
  } finally {
    if (icon) icon.style.animation = '';
    loadConfig();
    loadStats();
  }
}

// Load Configurations
async function loadConfig() {
  try {
    const r = await fetch('/api/config');
    const cfg = await r.json();
    document.getElementById('force_disable_thinking').checked = !!cfg.force_disable_thinking;
    document.getElementById('socks5_paid_direct').checked = !!cfg.socks5_paid_direct;
    effortData = cfg.reasoning_effort_map || {};
    socks5Data = cfg.socks5_proxies || [];
    capData = cfg.max_tokens_cap_per_model || {};
    keywordRulesData = (cfg.model_alias || []).map(r => ({
      keyword: r.keyword || '',
      target: r.target || '',
      match_type: r.match_type || 'contains',
      case_insensitive: r.case_insensitive !== false,
      enabled: r.enabled !== false
    }));
    document.getElementById('maxTokensCap').value = cfg.max_tokens_cap || '';
    document.getElementById('upstreamBaseURLs').value = (cfg.upstream_base_urls || []).join('\n');
    if (cfg.log_level) document.getElementById('logLevelSelect').value = cfg.log_level.toLowerCase();
    if (cfg.log_bodies !== undefined) document.getElementById('logBodiesCheck').checked = !!cfg.log_bodies;

    const mr = await fetch('/v1/models');
    const md = await mr.json();
    modelList = (md.data || []).map(m => m.id).sort();

    renderEffortTable();
    renderSocks5Table();
    renderCapTable();
    renderKeywordRuleTable();
    document.getElementById('activeSocks5').value = cfg.active_socks5 || '';
  } catch (e) {
    showToast('加载配置失败: ' + e.message, 'error');
  }
}

function modelSelectHtml(selected, fieldName = 'val') {
  let h = '<select data-field="' + fieldName + '" class="form-control form-select">';
  h += '<option value="">-- 选择模型 --</option>';
  let found = false;
  for (const m of modelList) {
    if (selected === m) found = true;
    h += '<option value="' + esc(m) + '"' + (selected === m ? ' selected' : '') + '>' + esc(m) + '</option>';
  }
  if (selected && !found) {
    h += '<option value="' + esc(selected) + '" selected>' + esc(selected) + ' (预设/自定义)</option>';
  }
  h += '</select>';
  return h;
}



// Effort Table
function renderEffortTable() {
  const tb = document.querySelector('#effortTable tbody');
  const ks = Object.keys(effortData);
  if (!ks.length) {
    tb.innerHTML = '<tr><td colspan="3" class="empty-state">暂无推理力度映射配置</td></tr>';
    return;
  }
  tb.innerHTML = ks.map(k => '<tr><td><input class="form-control" value="' + esc(k) + '" data-field="key"></td><td><input class="form-control" value="' + esc(effortData[k]) + '" data-field="val"></td><td style="text-align:center"><button class="btn btn-rose btn-icon" onclick="delEffort(this)" title="删除"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg></button></td></tr>').join('');
}

function addEffortRow() {
  const tb = document.querySelector('#effortTable tbody');
  if (tb.querySelector('.empty-state')) tb.innerHTML = '';
  tb.insertAdjacentHTML('beforeend', '<tr><td><input class="form-control" value="" placeholder="例如: xhigh" data-field="key"></td><td><input class="form-control" value="" placeholder="例如: max" data-field="val"></td><td style="text-align:center"><button class="btn btn-rose btn-icon" onclick="delEffort(this)" title="删除"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg></button></td></tr>');
}

function delEffort(btn) {
  const row = btn.closest('tr');
  const ki = row.querySelector('[data-field="key"]');
  if (ki && ki.value && effortData[ki.value]) delete effortData[ki.value];
  row.remove();
  if (!document.querySelectorAll('#effortTable tbody tr').length) {
    document.querySelector('#effortTable tbody').innerHTML = '<tr><td colspan="3" class="empty-state">暂无推理力度映射配置</td></tr>';
  }
}

function collectEfforts() {
  const r = {};
  document.querySelectorAll('#effortTable tbody tr').forEach(tr => {
    const k = tr.querySelector('[data-field="key"]');
    const v = tr.querySelector('[data-field="val"]');
    if (k && k.value.trim()) r[k.value.trim()] = v ? v.value.trim() : '';
  });
  effortData = r;
  return r;
}

// SOCKS5 Table
function renderSocks5Table() {
  const tb = document.querySelector('#socks5Table tbody');
  if (!socks5Data.length) {
    tb.innerHTML = '<tr><td colspan="5" class="empty-state">暂无 SOCKS5 代理节点</td></tr>';
    renderSocks5Select();
    return;
  }
  tb.innerHTML = socks5Data.map((p, i) => '<tr><td><input class="form-control" value="' + esc(p.name || '') + '" data-field="name" placeholder="名称 (例如 clash)"></td><td><input class="form-control" value="' + esc(p.addr) + '" data-field="addr" placeholder="127.0.0.1:7890"></td><td><input class="form-control" value="' + esc(p.username || '') + '" data-field="username" placeholder="可选"></td><td><input class="form-control" value="' + esc(p.password || '') + '" data-field="password" type="password" placeholder="可选"></td><td style="text-align:center"><button class="btn btn-rose btn-icon" onclick="delSocks5(' + i + ')" title="删除"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg></button></td></tr>').join('');
  renderSocks5Select();
}

function addSocks5Row() {
  const tb = document.querySelector('#socks5Table tbody');
  if (tb.querySelector('.empty-state')) tb.innerHTML = '';
  socks5Data.push({ addr: '', name: '' });
  renderSocks5Table();
}

function delSocks5(i) {
  socks5Data.splice(i, 1);
  renderSocks5Table();
}

function collectSocks5() {
  const r = [];
  document.querySelectorAll('#socks5Table tbody tr').forEach(tr => {
    const a = tr.querySelector('[data-field="addr"]');
    if (a && a.value.trim()) {
      r.push({
        addr: a.value.trim(),
        name: (tr.querySelector('[data-field="name"]') || {}).value?.trim() || '',
        username: (tr.querySelector('[data-field="username"]') || {}).value?.trim() || '',
        password: (tr.querySelector('[data-field="password"]') || {}).value?.trim() || ''
      });
    }
  });
  socks5Data = r;
  return r;
}

function renderSocks5Select() {
  const sel = document.getElementById('activeSocks5');
  const cur = sel.value;
  sel.innerHTML = '<option value="">直连 (不使用 SOCKS5 代理)</option>';
  socks5Data.forEach(p => {
    if (p.addr) {
      const label = p.name ? p.name + ' (' + p.addr + ')' : p.addr;
      const opt = document.createElement('option');
      opt.value = p.addr;
      opt.textContent = label;
      sel.appendChild(opt);
    }
  });
  if (socks5Data.length >= 2) {
    const opt = document.createElement('option');
    opt.value = '__round_robin__';
    opt.textContent = '轮询自动切换 (Round-Robin)';
    sel.appendChild(opt);
  }
  sel.value = cur;
  if (!sel.value) sel.value = '';
}

// Cap Table
function renderCapTable() {
  const tb = document.querySelector('#capTable tbody');
  const ks = Object.keys(capData);
  if (!ks.length) {
    tb.innerHTML = '<tr><td colspan="3" class="empty-state">暂无模型上限配置</td></tr>';
    return;
  }
  tb.innerHTML = ks.map(k => '<tr><td>' + modelSelectHtml(k, 'key') + '</td><td><input type="number" class="form-control" value="' + capData[k] + '" data-field="cap" min="0" placeholder="上限数值"></td><td style="text-align:center"><button class="btn btn-rose btn-icon" onclick="delCap(this)" title="删除"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg></button></td></tr>').join('');
}

function addCapRow() {
  const tb = document.querySelector('#capTable tbody');
  if (tb.querySelector('.empty-state')) tb.innerHTML = '';
  tb.insertAdjacentHTML('beforeend', '<tr><td>' + modelSelectHtml('', 'key') + '</td><td><input type="number" class="form-control" value="0" data-field="cap" min="0" placeholder="上限数值"></td><td style="text-align:center"><button class="btn btn-rose btn-icon" onclick="delCap(this)" title="删除"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg></button></td></tr>');
}

function collectCaps() {
  capData = {};
  document.querySelectorAll('#capTable tbody tr').forEach(tr => {
    const sel = tr.querySelector('[data-field="key"]');
    const inp = tr.querySelector('[data-field="cap"]');
    if (sel && inp) {
      const k = sel.value;
      if (k) {
        const v = parseInt(inp.value) || 0;
        capData[k] = v;
      }
    }
  });
  return capData;
}

function delCap(btn) {
  btn.closest('tr').remove();
  if (!document.querySelectorAll('#capTable tbody tr').length) {
    document.querySelector('#capTable tbody').innerHTML = '<tr><td colspan="3" class="empty-state">暂无模型上限配置</td></tr>';
  }
}


// Keyword Rules Table
function renderKeywordRuleTable() {
  const tb = document.querySelector('#keywordRuleTable tbody');
  if (!tb) return;
  if (!keywordRulesData.length) {
    tb.innerHTML = '<tr><td colspan="6" class="empty-state">暂无关键词映射规则，可点击上方快捷预设载入</td></tr>';
    return;
  }
  tb.innerHTML = keywordRulesData.map(function(r, i) {
    var h = '<tr data-index="' + i + '">';
    h += '<td style="text-align:center"><input type="checkbox" data-field="enabled"' + (r.enabled ? ' checked' : '') + ' onchange="onKeywordFieldChange()"></td>';
    h += '<td><select class="form-control form-select" data-field="match_type" onchange="onKeywordFieldChange()">';
    h += '<option value="contains"' + (r.match_type === 'contains' || !r.match_type ? ' selected' : '') + '>包含 (Contains)</option>';
    h += '<option value="exact"' + (r.match_type === 'exact' ? ' selected' : '') + '>精确 (Exact)</option>';
    h += '<option value="prefix"' + (r.match_type === 'prefix' ? ' selected' : '') + '>前缀 (Prefix)</option>';
    h += '<option value="regex"' + (r.match_type === 'regex' ? ' selected' : '') + '>正则 (Regex)</option>';
    h += '</select></td>';
    h += '<td><input class="form-control" value="' + esc(r.keyword) + '" data-field="keyword" placeholder="关键词如: sonnet" oninput="onKeywordFieldChange()"></td>';
    h += '<td>' + modelSelectHtml(r.target, 'target') + '</td>';
    h += '<td style="text-align:center"><input type="checkbox" data-field="case_insensitive"' + (r.case_insensitive ? ' checked' : '') + ' onchange="onKeywordFieldChange()"></td>';
    h += '<td style="text-align:center">';
    h += '<button type="button" class="btn btn-icon" onclick="moveKeywordRule(' + i + ', -1)" title="上移">▲</button> ';
    h += '<button type="button" class="btn btn-icon" onclick="moveKeywordRule(' + i + ', 1)" title="下移">▼</button> ';
    h += '<button type="button" class="btn btn-rose btn-icon" onclick="delKeywordRule(' + i + ')" title="删除"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg></button>';
    h += '</td></tr>';
    return h;
  }).join('');

  tb.querySelectorAll('select[data-field="target"]').forEach(function(sel) {
    sel.onchange = onKeywordFieldChange;
  });
}

function onKeywordFieldChange() {
  collectKeywordRules();
  simulateModelResolve();
}

function addKeywordRuleRow() {
  collectKeywordRules();
  keywordRulesData.push({
    keyword: '',
    target: '',
    match_type: 'contains',
    case_insensitive: true,
    enabled: true
  });
  renderKeywordRuleTable();
  simulateModelResolve();
}

function collectKeywordRules() {
  const rows = document.querySelectorAll('#keywordRuleTable tbody tr');
  const list = [];
  rows.forEach(tr => {
    if (tr.querySelector('.empty-state')) return;
    const enabled = tr.querySelector('[data-field="enabled"]')?.checked ?? true;
    const matchType = tr.querySelector('[data-field="match_type"]')?.value || 'contains';
    const keyword = tr.querySelector('[data-field="keyword"]')?.value.trim() || '';
    const target = tr.querySelector('[data-field="target"]')?.value.trim() || '';
    const caseInsensitive = tr.querySelector('[data-field="case_insensitive"]')?.checked ?? true;
    list.push({
      keyword: keyword,
      target: target,
      match_type: matchType,
      case_insensitive: caseInsensitive,
      enabled: enabled
    });
  });
  keywordRulesData = list;
  return keywordRulesData;
}

function delKeywordRule(index) {
  collectKeywordRules();
  keywordRulesData.splice(index, 1);
  renderKeywordRuleTable();
  simulateModelResolve();
}

function moveKeywordRule(index, delta) {
  collectKeywordRules();
  const targetIndex = index + delta;
  if (targetIndex < 0 || targetIndex >= keywordRulesData.length) return;
  const temp = keywordRulesData[index];
  keywordRulesData[index] = keywordRulesData[targetIndex];
  keywordRulesData[targetIndex] = temp;
  renderKeywordRuleTable();
  simulateModelResolve();
}

function addPresetRule(kw, target) {
  collectKeywordRules();
  if (keywordRulesData.some(r => r.keyword.toLowerCase() === kw.toLowerCase())) {
    showToast('规则 [' + kw + '] 已存在', 'warn');
    return;
  }
  keywordRulesData.push({
    keyword: kw,
    target: target,
    match_type: 'contains',
    case_insensitive: true,
    enabled: true
  });
  renderKeywordRuleTable();
  simulateModelResolve();
  showToast('已添加预设规则: ' + kw);
}

function loadAllPresets() {
  collectKeywordRules();
  let added = 0;
  KEYWORD_PRESETS.forEach(p => {
    if (!keywordRulesData.some(r => r.keyword.toLowerCase() === p.keyword.toLowerCase())) {
      keywordRulesData.push({ ...p });
      added++;
    }
  });
  renderKeywordRuleTable();
  simulateModelResolve();
  if (added > 0) {
    showToast('已成功导入 ' + added + ' 条预设规则');
  } else {
    showToast('所有预设规则均已存在', 'warn');
  }
}

function simulateModelResolve() {
  var el = document.getElementById('testModelInput');
  var raw = (el && el.value ? el.value : '').trim();
  var resBox = document.getElementById('testResultBox');
  if (!resBox) return;
  if (!raw) {
    resBox.innerHTML = '等待输入...';
    resBox.style.color = 'var(--text-sec)';
    return;
  }

  var base = raw;
  var suffix = '';
  var m = raw.match(/^(.+?)(\[[a-zA-Z0-9_-]+\])$/);
  if (m) {
    base = m[1];
    suffix = m[2];
  }

  collectKeywordRules();
  for (var i = 0; i < keywordRulesData.length; i++) {
    var r = keywordRulesData[i];
    if (!r.enabled || !r.keyword) continue;
    var hit = false;
    var tb = r.case_insensitive ? base.toLowerCase() : base;
    var kw = r.case_insensitive ? r.keyword.toLowerCase() : r.keyword;

    if (r.match_type === 'exact' && tb === kw) {
      hit = true;
    } else if (r.match_type === 'prefix' && tb.indexOf(kw) === 0) {
      hit = true;
    } else if (r.match_type === 'regex') {
      try {
        var re = new RegExp(r.keyword, r.case_insensitive ? 'i' : '');
        if (re.test(base)) hit = true;
      } catch (e) {}
    } else if (tb.indexOf(kw) !== -1) {
      hit = true;
    }

    if (hit) {
      resBox.innerHTML = '🎯 命中规则「<b>' + esc(r.keyword) + '</b>」 (' + esc(r.match_type) + ') ➔ 目标: <b style="color:var(--accent-color)">' + esc(r.target) + suffix + '</b>';
      resBox.style.color = '#10b981';
      return;
    }
  }

  resBox.innerHTML = '⚪ 未命中任何规则 ➔ 直通上游: <b>' + esc(raw) + '</b>';
  resBox.style.color = 'var(--text-sec)';
}

// Save All Configs
async function saveConfig() {
  collectEfforts();
  collectSocks5();
  collectCaps();
  collectKeywordRules();

  const logLevel = document.getElementById('logLevelSelect').value;
  const logBodies = document.getElementById('logBodiesCheck').checked;

  const cfg = {
    model_alias: keywordRulesData.filter(r => r.keyword && r.target),
    reasoning_effort_map: effortData,
    force_disable_thinking: document.getElementById('force_disable_thinking').checked,
    max_tokens_cap: parseInt(document.getElementById('maxTokensCap').value) || 0,
    max_tokens_cap_per_model: capData,
    socks5_proxies: socks5Data,
    active_socks5: document.getElementById('activeSocks5').value,
    socks5_paid_direct: document.getElementById('socks5_paid_direct').checked,
    upstream_base_urls: document.getElementById('upstreamBaseURLs').value.split('\n').map(s => s.trim()).filter(Boolean),
    log_level: logLevel,
    log_bodies: logBodies
  };

  try {
    const r = await fetch('/api/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(cfg)
    });
    if (!r.ok) throw new Error(await r.text());
    showToast('全部配置已成功保存并即时生效', 'success');
    loadConfig();
  } catch (e) {
    showToast('保存配置失败: ' + e.message, 'error');
  }
}

// Stats Loading & Filtering
async function loadStats() {
  try {
    const r = await fetch('/api/stats');
    const d = await r.json();
    allStatsData = d;
    renderStatsTable(d);
  } catch (e) {
    document.getElementById('statsTableBody').innerHTML = '<tr><td colspan="7" class="empty-state">获取统计数据失败</td></tr>';
  }
}

function renderStatsTable(d) {
  const ms = d.models || {};
  const ks = Object.keys(ms);
  const filter = (document.getElementById('statsFilterInput').value || '').toLowerCase().trim();

  let tr = 0, pt = 0, ct = 0, tt = 0, cr = 0, cw = 0;
  for (const k of ks) {
    const m = ms[k];
    tr += m.request_count || 0;
    pt += m.prompt_tokens || 0;
    ct += m.completion_tokens || 0;
    tt += m.total_tokens || 0;
    cr += m.cache_read_tokens || 0;
    cw += m.cache_created_tokens || 0;
  }

  // Update Hero HUD tiles
  document.getElementById('hudTotalRequests').textContent = fmt(d.total_requests || tr);
  document.getElementById('hudActiveModels').textContent = '活跃模型: ' + ks.length;
  document.getElementById('hudTotalTokens').textContent = fmtCompact(tt);
  document.getElementById('hudPromptCompletionSum').textContent = '总计 ' + fmt(tt) + ' Tokens';

  const pRatio = tt > 0 ? Math.round((pt / tt) * 100) : 0;
  const cRatio = tt > 0 ? Math.round((ct / tt) * 100) : 0;
  document.getElementById('hudRatioText').textContent = pRatio + '% / ' + cRatio + '%';
  document.getElementById('hudBarPrompt').style.width = pRatio + '%';
  document.getElementById('hudBarComp').style.width = cRatio + '%';
  document.getElementById('hudPromptTokensText').textContent = '入: ' + fmtCompact(pt);
  document.getElementById('hudCompTokensText').textContent = '出: ' + fmtCompact(ct);

  document.getElementById('hudCacheRead').textContent = fmtCompact(cr);
  document.getElementById('hudCacheWrite').textContent = '写入: ' + fmtCompact(cw);

  document.getElementById('statsModelCount').textContent = ks.length + ' MODELS';

  const filteredKeys = ks.filter(k => !filter || k.toLowerCase().includes(filter));

  let h = '';
  if (!filteredKeys.length) {
    h = '<tr><td colspan="7" class="empty-state">' + (filter ? '无匹配模型' : '暂无 Token 统计数据') + '</td></tr>';
  } else {
    for (const k of filteredKeys) {
      const m = ms[k];
      h += '<tr>';
      h += '<td style="font-weight:600">' + esc(k) + '</td>';
      h += '<td class="num-cell">' + fmt(m.request_count || 0) + '</td>';
      h += '<td class="num-cell">' + fmt(m.prompt_tokens || 0) + '</td>';
      h += '<td class="num-cell">' + fmt(m.completion_tokens || 0) + '</td>';
      h += '<td class="num-cell" style="font-weight:600;color:var(--text)">' + fmt(m.total_tokens || 0) + '</td>';
      h += '<td class="num-cell" style="color:var(--emerald)">' + fmt(m.cache_read_tokens || 0) + '</td>';
      h += '<td class="num-cell" style="color:var(--amber)">' + fmt(m.cache_created_tokens || 0) + '</td>';
      h += '</tr>';
    }
    if (!filter) {
      h += '<tr class="total-row">';
      h += '<td>总计 (' + ks.length + ' 个模型)</td>';
      h += '<td class="num-cell">' + fmt(tr) + '</td>';
      h += '<td class="num-cell">' + fmt(pt) + '</td>';
      h += '<td class="num-cell">' + fmt(ct) + '</td>';
      h += '<td class="num-cell">' + fmt(tt) + '</td>';
      h += '<td class="num-cell" style="color:var(--emerald)">' + fmt(cr) + '</td>';
      h += '<td class="num-cell" style="color:var(--amber)">' + fmt(cw) + '</td>';
      h += '</tr>';
    }
  }
  document.getElementById('statsTableBody').innerHTML = h;
}

function filterStatsTable() {
  if (allStatsData) renderStatsTable(allStatsData);
}

// Reset Stats Modal
function openResetModal() {
  document.getElementById('resetModal').classList.add('show');
}
function closeResetModal() {
  document.getElementById('resetModal').classList.remove('show');
}
async function confirmResetStats() {
  closeResetModal();
  try {
    const r = await fetch('/api/stats', { method: 'DELETE' });
    if (!r.ok) throw new Error(await r.text());
    showToast('Token 统计历史已清空', 'success');
    loadStats();
  } catch (e) {
    showToast('清空失败: ' + e.message, 'error');
  }
}

// Helpers
function esc(s) {
  const d = document.createElement('div');
  d.textContent = s || '';
  return d.innerHTML;
}

function fmt(n) {
  return (n || 0).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

function fmtCompact(n) {
  if (!n) return '0';
  if (n >= 1000000) return (n / 1000000).toFixed(2) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
  return n.toString();
}

function showToast(msg, type = 'success') {
  const t = document.getElementById('toast');
  const m = document.getElementById('toastMsg');
  const ic = document.getElementById('toastIcon');
  m.textContent = msg;
  t.className = type + ' show';
  if (type === 'success') {
    ic.innerHTML = '<polyline points="20 6 9 17 4 12"></polyline>';
  } else {
    ic.innerHTML = '<circle cx="12" cy="12" r="10"></circle><line x1="12" y1="8" x2="12" y2="12"></line><line x1="12" y1="16" x2="12.01" y2="16"></line>';
  }
  clearTimeout(t._tid);
  t._tid = setTimeout(() => t.classList.remove('show'), 3000);
}

// Global Keyboard Shortcut (Ctrl+S / Cmd+S)
window.addEventListener('keydown', function(e) {
  if ((e.ctrlKey || e.metaKey) && e.key === 's') {
    e.preventDefault();
    saveConfig();
  }
});

// Ambient Particles Canvas
(function() {
  const canvas = document.getElementById('ambient-canvas');
  if (!canvas) return;
  const ctx = canvas.getContext('2d');
  let width, height;
  const particles = [];
  const count = 42;

  function resize() {
    width = canvas.width = window.innerWidth;
    height = canvas.height = window.innerHeight;
  }
  window.addEventListener('resize', resize);
  resize();

  for (let i = 0; i < count; i++) {
    particles.push({
      x: Math.random() * width,
      y: Math.random() * height,
      vx: (Math.random() - 0.5) * 0.35,
      vy: (Math.random() - 0.5) * 0.35,
      radius: Math.random() * 1.5 + 0.8
    });
  }

  function render() {
    ctx.clearRect(0, 0, width, height);
    const isDark = document.documentElement.getAttribute('data-theme') !== 'light';
    const pColor = isDark ? 'rgba(99, 102, 241, 0.35)' : 'rgba(99, 102, 241, 0.2)';
    const lColor = isDark ? 'rgba(99, 102, 241, 0.05)' : 'rgba(99, 102, 241, 0.035)';

    for (let i = 0; i < particles.length; i++) {
      const p = particles[i];
      p.x += p.vx;
      p.y += p.vy;
      if (p.x < 0) p.x = width;
      if (p.x > width) p.x = 0;
      if (p.y < 0) p.y = height;
      if (p.y > height) p.y = 0;

      ctx.beginPath();
      ctx.arc(p.x, p.y, p.radius, 0, Math.PI * 2);
      ctx.fillStyle = pColor;
      ctx.fill();

      for (let j = i + 1; j < particles.length; j++) {
        const p2 = particles[j];
        const dx = p.x - p2.x;
        const dy = p.y - p2.y;
        const dist = Math.sqrt(dx * dx + dy * dy);
        if (dist < 140) {
          ctx.beginPath();
          ctx.moveTo(p.x, p.y);
          ctx.lineTo(p2.x, p2.y);
          ctx.strokeStyle = lColor;
          ctx.lineWidth = 1;
          ctx.stroke();
        }
      }
    }
    requestAnimationFrame(render);
  }
  render();
})();

window.onload = function() {
  loadConfig();
  loadStats();
};
setInterval(loadStats, 5000);
document.addEventListener('visibilitychange', function() {
  if (!document.hidden) loadStats();
});
</script>
</body>
</html>`
