// Copyright 2026 SandrPod
// Local-only HTTP settings page.
//
// Bound to 127.0.0.1 only. We rely on the loopback boundary for security —
// any local user could connect, but the macOS / Windows / Linux user
// boundary already restricts loopback ports to processes owned by the same
// user (more accurately: any process can connect, but in a single-user
// laptop this is acceptable; multi-user shared hosts will need to layer on
// CSRF protection in Sprint 4).
//
// Endpoints:
//   GET  /                  — HTML settings page
//   GET  /api/snapshot      — current rules + hardlocks JSON
//   POST /api/rules/add     — body: {"path":"...", "mode":"r|w|rw"}
//   POST /api/rules/rm      — body: {"path":"..."}

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/sandrpod/sandrpod/pkg/permission"
)

// httpIndex renders the settings page. We do server-side templating with the
// stdlib `html` escaper rather than pull in html/template — the page is
// small enough that the readability win of inlining is bigger than the cost
// of doing it manually.
func httpIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	snap := runStore.Snapshot()

	var rulesRows strings.Builder
	for _, ru := range snap.Rules {
		rulesRows.WriteString(renderRuleRow(ru))
	}
	if len(snap.Rules) == 0 {
		rulesRows.WriteString(`<tr><td colspan="5" class="empty">（暂无规则）</td></tr>`)
	}

	var sessionRows strings.Builder
	for _, ru := range snap.SessionGrants {
		sessionRows.WriteString(renderSessionRow(ru))
	}
	if len(snap.SessionGrants) == 0 {
		sessionRows.WriteString(`<tr><td colspan="5" class="empty">（无活跃会话授权）</td></tr>`)
	}

	var policyRows strings.Builder
	for _, c := range snap.CommandPolicy.Deny {
		policyRows.WriteString(renderPolicyRow(c, "deny"))
	}
	for _, c := range snap.CommandPolicy.Warn {
		policyRows.WriteString(renderPolicyRow(c, "warn"))
	}
	if len(snap.CommandPolicy.Deny)+len(snap.CommandPolicy.Warn) == 0 {
		policyRows.WriteString(`<tr><td colspan="3" class="empty">（暂无命令策略 — 重启 tray 会自动安装默认列表）</td></tr>`)
	}

	fmt.Fprintf(w, settingsHTML, rulesRows.String(), sessionRows.String(), policyRows.String())
}

func renderPolicyRow(cmd, action string) string {
	cls := "scope-permanent"
	if action == "deny" {
		cls = "scope-hardlock"
	}
	return fmt.Sprintf(
		`<tr class="%s"><td><span class="badge">%s</span></td><td><code>%s</code></td><td><button onclick="rmPolicy(%q)">移除</button></td></tr>`,
		cls, html.EscapeString(action), html.EscapeString(cmd), cmd,
	)
}

func httpPolicyUpsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Command string `json:"command"`
		Action  string `json:"action"` // "deny" or "warn"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Command == "" || (body.Action != "deny" && body.Action != "warn") {
		http.Error(w, "command and action (deny|warn) required", http.StatusBadRequest)
		return
	}
	cur := runStore.Snapshot().CommandPolicy
	// Match the CLI's upsert semantics: moving to one list removes from the other.
	if body.Action == "deny" {
		cur.Deny = appendUniqueStr(cur.Deny, body.Command)
		cur.Warn = removeStr(cur.Warn, body.Command)
	} else {
		cur.Warn = appendUniqueStr(cur.Warn, body.Command)
		cur.Deny = removeStr(cur.Deny, body.Command)
	}
	if err := runStore.SetCommandPolicy(cur); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func httpPolicyRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	cur := runStore.Snapshot().CommandPolicy
	cur.Deny = removeStr(cur.Deny, body.Command)
	cur.Warn = removeStr(cur.Warn, body.Command)
	if err := runStore.SetCommandPolicy(cur); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Local helpers (intentionally duplicated from policy_cli.go for HTTP path
// independence — keeps the two layers from coupling on each other).
func appendUniqueStr(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func removeStr(list []string, v string) []string {
	out := list[:0]
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func renderRuleRow(r permission.Rule) string {
	scopeBadge := html.EscapeString(string(r.Scope))
	cls := "scope-" + html.EscapeString(string(r.Scope))
	pathEsc := html.EscapeString(r.Path)
	modeEsc := html.EscapeString(string(r.Mode))
	noteEsc := html.EscapeString(r.Note)

	rmBtn := fmt.Sprintf(`<button onclick="rmRule(%q)">移除</button>`, r.Path)
	if r.Scope == permission.ScopeHardlock {
		rmBtn = `<span class="locked" title="hardlock 不能从 GUI 移除，请使用命令行 sandrpod-tray unlock --i-understand-the-risk">🔒 命令行解锁</span>`
	}

	return fmt.Sprintf(
		`<tr class="%s"><td><span class="badge">%s</span></td><td>%s</td><td><code>%s</code></td><td>%s</td><td>%s</td></tr>`,
		cls, scopeBadge, modeEsc, pathEsc, noteEsc, rmBtn,
	)
}

func renderSessionRow(r permission.Rule) string {
	return fmt.Sprintf(
		`<tr><td><span class="badge">session</span></td><td>%s</td><td><code>%s</code></td><td>%s</td><td>过期：%s</td></tr>`,
		html.EscapeString(string(r.Mode)),
		html.EscapeString(r.Path),
		html.EscapeString(r.SessionID),
		html.EscapeString(r.ExpiresAt.Format("2006-01-02 15:04")),
	)
}

// httpSnapshot returns the live snapshot as JSON for any future SPA upgrade.
func httpSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runStore.Snapshot())
}

// httpRuleAdd adds a permanent rule.
func httpRuleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	mode := permission.Mode(strings.ToLower(body.Mode))
	switch mode {
	case permission.ModeRead, permission.ModeWrite, permission.ModeReadWrite:
	default:
		http.Error(w, "mode must be r, w, or rw", http.StatusBadRequest)
		return
	}
	if err := runStore.AddPermanentRule(permission.Rule{Path: body.Path, Mode: mode}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// httpRuleRemove removes a permanent rule (refusing hardlocks — those need
// the CLI). The 403 we return for hardlocks intentionally surfaces in the
// browser console so the UI can show a hint.
func httpRuleRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// Pre-check that the targeted rule isn't a hardlock so we can return a
	// dedicated 403 instead of a silent no-op.
	for _, ru := range runStore.Snapshot().Rules {
		if ru.Path == body.Path && ru.Scope == permission.ScopeHardlock {
			http.Error(w, "cannot remove hardlock from GUI; use `sandrpod-tray unlock --i-understand-the-risk`", http.StatusForbidden)
			return
		}
	}
	if err := runStore.RemoveRule(body.Path, false); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// settingsHTML is a minimal-but-presentable single-page UI. Two %s
// placeholders: rules table body, session-grants table body. JS does the
// add/remove via fetch().
const settingsHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>Acme Sandbox 授权管理</title>
<style>
  body { font: 14px/1.5 -apple-system, "Segoe UI", "PingFang SC", sans-serif;
         margin: 32px auto; max-width: 880px; color: #1a1a1a; padding: 0 16px; }
  h1 { font-size: 22px; margin-bottom: 4px; }
  .sub { color: #666; margin-bottom: 24px; }
  h2 { font-size: 15px; margin-top: 32px; padding-bottom: 4px; border-bottom: 1px solid #eee; }
  table { width: 100%%; border-collapse: collapse; margin-top: 8px; }
  th, td { padding: 8px 10px; text-align: left; border-bottom: 1px solid #f0f0f0; vertical-align: top; }
  th { font-weight: 600; color: #555; font-size: 12px; text-transform: uppercase; letter-spacing: 0.04em; }
  code { background: #f5f5f7; padding: 1px 6px; border-radius: 3px; font-size: 12px; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 10px; font-size: 11px;
           background: #eaeaea; color: #444; }
  .scope-hardlock .badge { background: #ffe2dc; color: #b6320e; }
  .scope-permanent .badge { background: #e0f2e7; color: #1f7a3d; }
  .empty { color: #999; text-align: center; padding: 16px; }
  .locked { color: #b6320e; font-size: 12px; }
  button { background: #fff; border: 1px solid #ccc; border-radius: 4px;
           padding: 4px 10px; cursor: pointer; font-size: 12px; }
  button:hover { background: #f5f5f5; }
  button.primary { background: #1a73e8; color: #fff; border-color: #1a73e8; }
  button.primary:hover { background: #1664ce; }
  form { display: flex; gap: 8px; margin-top: 12px; }
  form input[type=text] { flex: 1; padding: 6px 10px; border: 1px solid #ccc; border-radius: 4px; }
  form select { padding: 6px; border: 1px solid #ccc; border-radius: 4px; }
  .footer { margin-top: 40px; color: #888; font-size: 12px; }
</style>
</head>
<body>
  <h1>Acme Sandbox 授权管理</h1>
  <p class="sub">本页面仅在你的电脑上运行（127.0.0.1）。任何关闭浏览器后规则仍然生效，规则文件位于 <code>~/.sandrpod/permissions.json</code>。</p>

  <h2>持久化规则</h2>
  <table>
    <thead><tr><th>类型</th><th>模式</th><th>路径</th><th>备注</th><th>操作</th></tr></thead>
    <tbody>%s</tbody>
  </table>

  <h2>新增规则</h2>
  <form onsubmit="addRule(event)">
    <input type="text" id="newPath" placeholder="路径，例如 ~/Documents 或 /Users/me/code" required>
    <select id="newMode">
      <option value="r">只读</option>
      <option value="w">只写</option>
      <option value="rw" selected>读写</option>
    </select>
    <button type="submit" class="primary">添加</button>
  </form>

  <h2>会话授权（自动过期）</h2>
  <table>
    <thead><tr><th>类型</th><th>模式</th><th>路径</th><th>会话</th><th>有效期</th></tr></thead>
    <tbody>%s</tbody>
  </table>

  <h2>命令策略</h2>
  <p class="sub" style="margin-bottom:8px;">deny：AI 提交的代码字面量包含此命令时直接拒绝；warn：仅记录审计，允许执行。仅匹配 <code>argv[0]</code> 的 basename，对 <code>eval</code> / <code>base64</code> 解码等绕过手段不构成安全边界。</p>
  <table>
    <thead><tr><th>动作</th><th>命令</th><th>操作</th></tr></thead>
    <tbody>%s</tbody>
  </table>
  <form onsubmit="addPolicy(event)" style="margin-top:12px;">
    <input type="text" id="newCmd" placeholder="命令名称，例如 scp" required>
    <select id="newAction">
      <option value="deny">deny（拒绝执行）</option>
      <option value="warn">warn（仅记录）</option>
    </select>
    <button type="submit" class="primary">添加</button>
  </form>

  <p class="footer">硬锁（hardlock）路径默认禁止 AI 访问，且无法通过此页面解除。如需开放访问，请在终端执行：
    <code>sandrpod-tray unlock &lt;path&gt; --i-understand-the-risk</code></p>

<script>
async function addRule(e) {
  e.preventDefault();
  const path = document.getElementById('newPath').value.trim();
  const mode = document.getElementById('newMode').value;
  if (!path) return;
  const r = await fetch('/api/rules/add', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path, mode }),
  });
  if (r.ok) { location.reload(); }
  else { alert('添加失败：' + (await r.text())); }
}
async function rmRule(path) {
  if (!confirm('确认移除：' + path + ' ?')) return;
  const r = await fetch('/api/rules/rm', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ path }),
  });
  if (r.ok) { location.reload(); }
  else { alert('移除失败：' + (await r.text())); }
}
async function addPolicy(e) {
  e.preventDefault();
  const command = document.getElementById('newCmd').value.trim();
  const action = document.getElementById('newAction').value;
  if (!command) return;
  const r = await fetch('/api/policy/upsert', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ command, action }),
  });
  if (r.ok) { location.reload(); }
  else { alert('添加失败：' + (await r.text())); }
}
async function rmPolicy(command) {
  if (!confirm('确认移除命令策略：' + command + ' ?')) return;
  const r = await fetch('/api/policy/rm', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ command }),
  });
  if (r.ok) { location.reload(); }
  else { alert('移除失败：' + (await r.text())); }
}
</script>
</body>
</html>`
