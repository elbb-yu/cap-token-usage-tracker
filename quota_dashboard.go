package main

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaDashboardHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>API Key 额度</title>
<style>
:root{color-scheme:light dark;--bg:#f5f7fb;--panel:#fff;--text:#172033;--muted:#697386;--line:#dde3ec;--accent:#2563eb;--danger:#dc2626;--ok:#059669;--warn:#d97706}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}.shell{max-width:1180px;margin:0 auto;padding:28px 18px 50px}.top{display:flex;align-items:flex-start;justify-content:space-between;gap:18px;margin-bottom:20px}h1{margin:0 0 6px;font-size:28px}.sub{color:var(--muted);max-width:780px}.actions{display:flex;gap:9px;flex-wrap:wrap}button,a.button{border:1px solid var(--line);border-radius:9px;background:var(--panel);color:var(--text);padding:9px 13px;text-decoration:none;cursor:pointer;font-weight:650}button.primary{background:var(--accent);border-color:var(--accent);color:white}button.danger{color:var(--danger)}button:disabled{opacity:.55;cursor:not-allowed}.notice{margin:0 0 16px;padding:12px 14px;border:1px solid #f0c36d;border-radius:10px;background:color-mix(in srgb,#fef3c7 70%,var(--panel));color:color-mix(in srgb,var(--text) 80%,#92400e)}.error{display:none;margin:0 0 14px;padding:11px 13px;border:1px solid #fecaca;border-radius:9px;background:#fef2f2;color:#991b1b}.error.show{display:block}.panel{overflow:hidden;border:1px solid var(--line);border-radius:13px;background:var(--panel);box-shadow:0 4px 16px rgb(15 23 42/.05)}table{width:100%;border-collapse:collapse}th,td{padding:13px 14px;border-bottom:1px solid var(--line);text-align:left;white-space:nowrap}th{color:var(--muted);font-size:12px;font-weight:750;background:color-mix(in srgb,var(--panel) 88%,var(--bg))}tr:last-child td{border-bottom:0}.key{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}.money{font-variant-numeric:tabular-nums}.usage{display:flex;align-items:center;gap:9px}.bar{width:130px;height:8px;border-radius:9px;background:var(--line);overflow:hidden}.fill{height:100%;background:var(--ok);transition:width .25s ease}.fill.warn{background:var(--warn)}.fill.danger{background:var(--danger)}.percent{min-width:45px;color:var(--muted);font-variant-numeric:tabular-nums}.status{font-weight:700}.status.ok{color:var(--ok)}.status.warn{color:var(--warn)}.status.danger{color:var(--danger)}.row-actions{display:flex;gap:7px}.row-actions button{padding:6px 9px;font-size:12px}.empty{padding:38px;text-align:center;color:var(--muted)}.foot{margin-top:12px;color:var(--muted);font-size:12px}@media(max-width:850px){.top{display:block}.actions{margin-top:14px}.panel{overflow:auto}}
@media(prefers-color-scheme:dark){:root{--bg:#0d1117;--panel:#161b22;--text:#e6edf3;--muted:#8b949e;--line:#30363d}.error{background:#2d1518;color:#ffb4b4;border-color:#6e3038}}
</style></head>
<body><main class="shell">
<div class="top"><div><h1>API Key 费用与额度</h1><div class="sub">按每次请求实际使用的模型和 Token 类型计算。查看不需要管理密钥；新增、修改和重置额度需要 CLIProxyAPI 管理密钥。</div></div><div class="actions"><a class="button" href="__DASHBOARD__">返回用量面板</a><button id="refresh">刷新</button><button id="add" class="primary">添加 / 设置 Key 额度</button></div></div>
<p class="notice">这里显示的是按 API 公开价格计算的等价费用/内部核算费用；使用 Codex Pro OAuth 时，它不等于一笔额外的 OpenAI API 账单。有限额 Key 遇到未定价模型会被暂停，防止通过换模型绕过额度。</p>
<div id="error" class="error"></div><section class="panel"><div id="content" class="empty">正在读取…</div></section><div id="foot" class="foot"></div>
</main>
<script>
const statusURL='__STATUS__',manageURL='__MANAGE__',resetURL='__RESET__';let current=[];
function usd(n){return '$'+Number(n||0).toFixed(4)}function esc(v){return String(v==null?'':v).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]))}
function error(message){const el=document.getElementById('error');el.textContent=message||'';el.classList.toggle('show',!!message)}
async function json(url,options){const response=await fetch(url,Object.assign({cache:'no-store'},options||{}));const payload=await response.json().catch(()=>({}));if(!response.ok)throw new Error(payload.error||('HTTP '+response.status));return payload}
function render(data){current=Array.isArray(data.items)?data.items:[];const root=document.getElementById('content');if(!current.length){root.className='empty';root.innerHTML='还没有观察到 API Key。Key 至少成功产生一条用量记录后会自动出现，也可以点击“添加 / 设置 Key 额度”。';return}root.className='';let html='<table><thead><tr><th>API Key</th><th>标签</th><th>实际费用</th><th>最高额度</th><th>剩余额度</th><th>使用进度</th><th>价格覆盖</th><th>状态</th><th>操作</th></tr></thead><tbody>';current.forEach(item=>{const limited=!!item.limited,pct=limited&&item.limit_usd>0?Math.min(100,item.used_usd/item.limit_usd*100):0,barClass=pct>=100?'danger':pct>=80?'warn':'',missing=Number(item.unpriced_requests||0),blocked=!!item.blocked;html+='<tr><td class="key">'+esc(item.masked_key||'未识别')+'</td><td>'+esc(item.label||'—')+'</td><td class="money">'+usd(item.used_usd)+'</td><td class="money">'+(limited?usd(item.limit_usd):'无限制')+'</td><td class="money">'+(limited?usd(item.remaining_usd):'—')+'</td><td>'+(limited?'<div class="usage"><div class="bar"><div class="fill '+barClass+'" style="width:'+pct.toFixed(1)+'%"></div></div><span class="percent">'+pct.toFixed(1)+'%</span></div>':'—')+'</td><td>'+(missing?'<span class="status warn">缺 '+missing+' 次</span>':'<span class="status ok">完整</span>')+'</td><td><span class="status '+(blocked?'danger':limited?'ok':'warn')+'">'+esc(blocked?(item.block_reason||'已暂停'):limited?'可用':'未限额')+'</span></td><td><div class="row-actions">'+(limited?'<button data-edit="'+esc(item.id)+'">修改额度</button><button class="danger" data-reset="'+esc(item.id)+'">已用置零</button>':'<button data-set="'+esc(item.id)+'">设置额度</button>')+'</div></td></tr>'});html+='</tbody></table>';root.innerHTML=html;document.getElementById('foot').textContent='更新时间：'+new Date(data.generated_at).toLocaleString()+' · 每 5 秒自动更新 · 费用基准：当前模型价格簿';}
async function load(){error('');try{render(await json(statusURL))}catch(e){error(e.message)}finally{document.getElementById('refresh').disabled=false}}
async function authorized(url,method,body){const key=prompt('请输入 CLIProxyAPI 管理密钥（仅用于本次操作）');if(!key)return null;return json(url,{method,headers:{'Content-Type':'application/json','Authorization':'Bearer '+key.trim()},body:JSON.stringify(body)})}
async function addQuota(){const apiKey=prompt('请输入要设置额度的完整 API Key。插件不会保存明文：');if(!apiKey)return;const label=prompt('标签（可留空）：','')||'';const raw=prompt('最高额度（美元，例如 20）：','20');if(raw==null)return;const limit=Number(raw);if(!Number.isFinite(limit)||limit<=0){error('额度必须是大于 0 的数字');return}await authorized(manageURL,'PUT',{api_key:apiKey.trim(),label:label.trim(),limit_usd:limit});await load()}
async function setObservedQuota(id){const item=current.find(x=>x.id===id);if(!item)return;const label=prompt('标签（可留空）：',item.label||'');if(label==null)return;const raw=prompt('最高额度（美元，例如 20）：','20');if(raw==null)return;const limit=Number(raw);if(!Number.isFinite(limit)||limit<=0){error('额度必须是大于 0 的数字');return}await authorized(manageURL,'PUT',{id,label:label.trim(),limit_usd:limit});await load()}
async function editQuota(id){const item=current.find(x=>x.id===id);if(!item)return;const label=prompt('标签（可留空）：',item.label||'');if(label==null)return;const raw=prompt('新的最高额度（美元）：',String(item.limit_usd));if(raw==null)return;const limit=Number(raw);if(!Number.isFinite(limit)||limit<=0){error('额度必须是大于 0 的数字');return}await authorized(manageURL,'PUT',{id,label:label.trim(),limit_usd:limit});await load()}
async function resetQuota(id){const item=current.find(x=>x.id===id);if(!item||!confirm('确认把 '+(item.label||item.masked_key)+' 的当前已用额度置为 $0？历史 Token 记录不会删除，额度将从现在重新累计。'))return;const result=await authorized(resetURL,'POST',{id});if(result)await load()}
document.getElementById('refresh').onclick=()=>{document.getElementById('refresh').disabled=true;load()};document.getElementById('add').onclick=()=>addQuota().catch(e=>error(e.message));document.getElementById('content').onclick=e=>{const edit=e.target.closest('[data-edit]'),reset=e.target.closest('[data-reset]'),set=e.target.closest('[data-set]');if(edit)editQuota(edit.dataset.edit).catch(x=>error(x.message));else if(reset)resetQuota(reset.dataset.reset).catch(x=>error(x.message));else if(set)setObservedQuota(set.dataset.set).catch(x=>error(x.message))};load();setInterval(()=>{if(document.visibilityState==='visible')load()},5000);
</script></body></html>`

func quotaDashboardResponse(routes registeredRoutes) pluginapi.ManagementResponse {
	html := strings.NewReplacer(
		"__DASHBOARD__", routes.dashboardPath,
		"__STATUS__", routes.resourceQuotasPath,
		"__MANAGE__", routes.quotasPath,
		"__RESET__", routes.quotaResetPath,
	).Replace(quotaDashboardHTML)
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"text/html; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: []byte(html),
	}
}
