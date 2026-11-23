<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'

type Account = { id: string; name: string; base_url: string; public_model: string; upstream_model: string; enabled: boolean; priority: number; api_key_set: boolean; ttft_ewma_ms: number; total_ewma_ms: number; samples: number; successes: number; failures: number; consecutive_failures: number; cooldown_until?: string; last_error?: string }
type Pool = { public_model: string; strategy: string; timeout_ms: number; request_timeout_ms: number; max_attempts: number; failure_threshold: number; cooldown_seconds: number; probe_interval_seconds: number }
type RequestItem = { at: string; model: string; account: string; status: number; ttft_ms: number; total_ms: number; attempts: number; error_type: string; error: string }

const adminPassword = ref(sessionStorage.getItem('keylane-admin') || '')
const signedIn = ref(false)
const error = ref('')
const busy = ref(false)
const tab = ref('overview')
const overview = ref({ accounts: 0, enabled: 0, healthy: 0, cooling: 0, model_pools: 0 })
const accounts = ref<Account[]>([])
const pools = ref<Pool[]>([])
const requests = ref<RequestItem[]>([])
const showAdd = ref(false)
const saving = ref(false)
const form = ref({ name: '', base_url: '', api_key: '', public_model: '', upstream_model: '', priority: 100 })
const models = computed(() => [...new Set(accounts.value.map(a => a.public_model))].sort())

async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('X-Admin-Password', adminPassword.value)
  if (init.body) headers.set('Content-Type', 'application/json')
  const response = await fetch(path, { ...init, headers })
  if (!response.ok) {
    let message = `Request failed (${response.status})`
    try { const body = await response.json(); message = body.error || body.message || message } catch {}
    throw new Error(message)
  }
  if (response.status === 204) return undefined as T
  return response.json()
}

async function loadAll() {
  busy.value = true; error.value = ''
  try {
    const [ov, ac, po, req] = await Promise.all([
      api<typeof overview.value>('/admin/api/overview'),
      api<{ data: Account[] }>('/admin/api/accounts'),
      api<{ data: Pool[] }>('/admin/api/model-pools'),
      api<{ data: RequestItem[] }>('/admin/api/requests'),
    ])
    overview.value = ov
    accounts.value = ac.data; pools.value = po.data; requests.value = req.data
    signedIn.value = true; sessionStorage.setItem('keylane-admin', adminPassword.value)
  } catch (e) { signedIn.value = false; sessionStorage.removeItem('keylane-admin'); error.value = e instanceof Error ? e.message : '连接失败' }
  finally { busy.value = false }
}

async function addAccount() {
  saving.value = true; error.value = ''
  try { await api('/admin/api/accounts', { method: 'POST', body: JSON.stringify(form.value) }); showAdd.value = false; form.value = { name: '', base_url: '', api_key: '', public_model: '', upstream_model: '', priority: 100 }; await loadAll() }
  catch (e) { error.value = e instanceof Error ? e.message : '保存失败' }
  finally { saving.value = false }
}

async function toggleAccount(a: Account) { await api(`/admin/api/accounts/${a.id}`, { method: 'PUT', body: JSON.stringify({ enabled: !a.enabled }) }); await loadAll() }
async function savePriority(a: Account) { try { await api(`/admin/api/accounts/${a.id}`, { method: 'PUT', body: JSON.stringify({ priority: a.priority }) }); ElMessage.success('账号顺序已保存'); await loadAll() } catch (e) { error.value = e instanceof Error ? e.message : '保存失败' } }
async function deleteAccount(a: Account) { try { await ElMessageBox.confirm(`删除账号“${a.name}”？`, '确认操作', { type: 'warning' }); await api(`/admin/api/accounts/${a.id}`, { method: 'DELETE' }); await loadAll() } catch {} }
async function probe(a: Account) { try { await api(`/admin/api/accounts/${a.id}/probe`, { method: 'POST' }); ElMessage.success('测速任务已排队'); } catch (e) { error.value = e instanceof Error ? e.message : '测速失败' } }
async function savePool(p: Pool) { try { await api('/admin/api/model-pools', { method: 'PUT', body: JSON.stringify(p) }); ElMessage.success(`${p.public_model} 策略已保存`); await loadAll() } catch (e) { error.value = e instanceof Error ? e.message : '保存失败' } }
function logout() { signedIn.value = false; adminPassword.value = ''; sessionStorage.removeItem('keylane-admin') }
function fmt(ms: number) { return ms > 0 ? `${Math.round(ms)} ms` : '—' }
function ago(date: string) { if (!date) return '—'; return new Date(date).toLocaleString() }

onMounted(() => { if (adminPassword.value) loadAll() })
</script>

<template>
  <div v-if="!signedIn" class="login-shell">
    <div class="login-card">
      <div class="brand-mark">K</div><p class="eyebrow">PERSONAL MODEL GATEWAY</p><h1>KeyLane</h1>
      <p class="muted">统一管理模型账号，自动选择近期响应最快的可用线路。</p>
      <el-form @submit.prevent="loadAll"><el-form-item label="管理密码"><el-input v-model="adminPassword" type="password" show-password placeholder="输入 ADMIN_PASSWORD" @keyup.enter="loadAll" /></el-form-item><el-button type="primary" class="full" :loading="busy" @click="loadAll">进入控制台</el-button></el-form>
      <p v-if="error" class="error-text">{{ error }}</p>
    </div>
  </div>
  <div v-else class="app-shell">
    <aside class="sidebar"><div class="side-brand"><div class="brand-mark small">K</div><div><b>KeyLane</b><small>MODEL GATEWAY</small></div></div>
      <div class="nav-label">控制台</div>
      <button v-for="item in [{id:'overview',label:'总览',icon:'◫'},{id:'accounts',label:'上游账号',icon:'⇄'},{id:'pools',label:'模型路由',icon:'◎'},{id:'requests',label:'请求记录',icon:'≋'}]" :key="item.id" class="nav-item" :class="{active:tab===item.id}" @click="tab=item.id"><span>{{ item.icon }}</span>{{ item.label }}</button>
      <div class="sidebar-bottom"><div class="online-dot"></div><span>本地网关运行中</span><button class="logout" @click="logout">退出</button></div>
    </aside>
    <main class="main-area"><header class="topbar"><div><span class="breadcrumb">KeyLane</span><span class="slash">/</span><strong>{{ ({overview:'总览',accounts:'上游账号',pools:'模型路由',requests:'请求记录'} as Record<string,string>)[tab] }}</strong></div><div class="top-actions"><span class="status-pill"><i></i> 服务正常</span><el-button circle plain @click="loadAll" :loading="busy">↻</el-button></div></header>
      <div class="content">
        <div v-if="error && signedIn" class="error-banner"><span>{{ error }}</span><button @click="error=''">×</button></div>
        <template v-if="tab==='overview'">
          <div class="page-heading"><div><p class="eyebrow">OVERVIEW</p><h1>运行总览</h1><p class="muted">查看账号健康与路由状态。测速与统计在后台运行，不阻塞模型请求。</p></div><el-button type="primary" @click="tab='accounts';showAdd=true">＋ 添加上游账号</el-button></div>
          <div class="stat-grid"><div class="stat-card"><div class="stat-label">上游账号</div><strong>{{ overview.accounts }}</strong><small>已启用 {{ overview.enabled }}</small><span class="stat-icon blue">⇄</span></div><div class="stat-card"><div class="stat-label">可用账号</div><strong>{{ overview.healthy }}</strong><small>当前可参与路由</small><span class="stat-icon green">✓</span></div><div class="stat-card"><div class="stat-label">冷却中</div><strong>{{ overview.cooling }}</strong><small>到期后重新参与</small><span class="stat-icon amber">◷</span></div><div class="stat-card"><div class="stat-label">模型池</div><strong>{{ overview.model_pools }}</strong><small>按公开模型隔离选路</small><span class="stat-icon violet">◎</span></div></div>
          <section class="panel"><div class="panel-heading"><div><h2>账号响应速度</h2><p>基于近期真实请求与定时轻量探测的 EWMA 估算</p></div><el-button text @click="tab='accounts'">全部账号 →</el-button></div><div v-if="accounts.length" class="table-wrap"><table><thead><tr><th>账号 / 模型</th><th>状态</th><th>首字延迟</th><th>完整响应</th><th>样本</th><th>最近错误</th></tr></thead><tbody><tr v-for="a in accounts.slice(0,6)" :key="a.id"><td><b>{{ a.name }}</b><small>{{ a.public_model }} · {{ a.upstream_model }}</small></td><td><span class="badge" :class="a.enabled ? (a.cooldown_until && Date.parse(a.cooldown_until)>Date.now()?'warn':'ok'):'off'">{{ !a.enabled?'已停用':a.cooldown_until && Date.parse(a.cooldown_until)>Date.now()?'冷却中':'可用' }}</span></td><td>{{ fmt(a.ttft_ewma_ms) }}</td><td>{{ fmt(a.total_ewma_ms) }}</td><td>{{ a.samples }}</td><td class="dim">{{ a.last_error || '—' }}</td></tr></tbody></table></div><el-empty v-else description="还没有账号，添加一个上游账号开始使用" /></section>
          <section class="panel"><div class="panel-heading"><div><h2>最近请求</h2><p>默认只保留请求元数据，不记录提示词和回答正文</p></div><el-button text @click="tab='requests'">查看记录 →</el-button></div><div v-if="requests.length" class="table-wrap"><table><thead><tr><th>时间</th><th>模型</th><th>账号</th><th>状态</th><th>首字 / 完成</th><th>尝试</th></tr></thead><tbody><tr v-for="(r,i) in requests.slice(0,5)" :key="i"><td>{{ ago(r.at) }}</td><td>{{ r.model }}</td><td>{{ r.account || '—' }}</td><td><span class="badge" :class="r.status>=200&&r.status<300?'ok':'warn'">{{ r.status || r.error_type || '—' }}</span></td><td>{{ fmt(r.ttft_ms) }} / {{ fmt(r.total_ms) }}</td><td>{{ r.attempts }}</td></tr></tbody></table></div><el-empty v-else description="暂无请求记录" /></section>
        </template>
        <template v-else-if="tab==='accounts'">
          <div class="page-heading"><div><p class="eyebrow">UPSTREAMS</p><h1>上游账号</h1><p class="muted">同一公开模型可加入多个 API Key 账号，网关按模型池策略选路。</p></div><el-button type="primary" @click="showAdd=true">＋ 添加账号</el-button></div>
          <section class="panel"><div class="table-wrap"><table><thead><tr><th>账号</th><th>公开模型</th><th>上游模型</th><th>状态</th><th>首包延迟</th><th>完整耗时</th><th>样本</th><th>顺序优先级</th><th>操作</th></tr></thead><tbody><tr v-for="a in accounts" :key="a.id"><td><b>{{ a.name }}</b><small>{{ a.base_url }}</small></td><td><code>{{ a.public_model }}</code></td><td><code>{{ a.upstream_model }}</code></td><td><span class="badge" :class="a.enabled ? (a.cooldown_until && Date.parse(a.cooldown_until)>Date.now()?'warn':'ok'):'off'">{{ !a.enabled?'已停用':a.cooldown_until && Date.parse(a.cooldown_until)>Date.now()?'冷却中':'可用' }}</span></td><td>{{ fmt(a.ttft_ewma_ms) }}</td><td>{{ fmt(a.total_ewma_ms) }}</td><td>{{ a.samples }}</td><td><el-input-number v-model="a.priority" :min="1" :max="9999" size="small" @change="savePriority(a)" /></td><td><div class="actions"><el-button link type="primary" @click="probe(a)">测速</el-button><el-button link @click="toggleAccount(a)">{{ a.enabled?'停用':'启用' }}</el-button><el-button link type="danger" @click="deleteAccount(a)">删除</el-button></div></td></tr></tbody></table></div><el-empty v-if="!accounts.length" description="还没有上游账号" /></section>
        </template>
        <template v-else-if="tab==='pools'">
          <div class="page-heading"><div><p class="eyebrow">ROUTING</p><h1>模型路由</h1><p class="muted">每个公开模型独立配置自动择优或按账号优先级顺序切换。</p></div></div>
          <div v-for="p in pools" :key="p.public_model" class="pool-card"><div class="pool-head"><div class="model-avatar">{{ p.public_model.slice(0,1).toUpperCase() }}</div><div><h2>{{ p.public_model }}</h2><p>{{ accounts.filter(a=>a.public_model===p.public_model).length }} 个账号 · 仅在此模型池内切换</p></div><el-tag :type="p.strategy==='fastest'?'success':'info'">{{ p.strategy==='fastest'?'自动择优':'按顺序' }}</el-tag></div><el-divider /><div class="pool-fields"><label>切换策略<el-select v-model="p.strategy"><el-option label="自动择优（近期响应快优先）" value="fastest"/><el-option label="按优先级顺序尝试" value="ordered"/></el-select></label><label>单次尝试超时（毫秒）<el-input-number v-model="p.timeout_ms" :min="1000" :max="3600000" :step="5000" /><small>约 {{ Math.round(p.timeout_ms/1000) }} 秒</small></label><label>整次请求超时（毫秒）<el-input-number v-model="p.request_timeout_ms" :min="1000" :max="7200000" :step="10000" /><small>含切换重试和流式响应</small></label><label>最多尝试账号数<el-input-number v-model="p.max_attempts" :min="1" :max="20" /></label><label>连续失败阈值<el-input-number v-model="p.failure_threshold" :min="1" :max="20" /></label><label>冷却时间（秒）<el-input-number v-model="p.cooldown_seconds" :min="1" :max="86400" /></label><label>测速间隔（秒，0 表示关闭）<el-input-number v-model="p.probe_interval_seconds" :min="0" :max="86400" :step="30" /></label></div><div class="pool-foot"><span class="muted">配置失败时继续使用最近有效路由配置。</span><el-button type="primary" @click="savePool(p)">保存路由</el-button></div></div>
          <el-empty v-if="!pools.length" description="添加账号后会自动创建模型池" />
        </template>
        <template v-else>
          <div class="page-heading"><div><p class="eyebrow">REQUESTS</p><h1>请求记录</h1><p class="muted">最近 100 条元数据记录，不含请求与响应正文。</p></div></div>
          <section class="panel"><div class="table-wrap"><table><thead><tr><th>时间</th><th>模型</th><th>实际账号</th><th>状态 / 错误</th><th>首字延迟</th><th>完整响应</th><th>尝试数</th></tr></thead><tbody><tr v-for="(r,i) in requests" :key="i"><td>{{ ago(r.at) }}</td><td><code>{{ r.model }}</code></td><td>{{ r.account || '—' }}</td><td><span class="badge" :class="r.status>=200&&r.status<300?'ok':'warn'">{{ r.status || r.error_type || '—' }}</span><small v-if="r.error" class="error-small">{{ r.error }}</small></td><td>{{ fmt(r.ttft_ms) }}</td><td>{{ fmt(r.total_ms) }}</td><td>{{ r.attempts }}</td></tr></tbody></table></div><el-empty v-if="!requests.length" description="暂无请求" /></section>
        </template>
      </div>
    </main>
    <el-dialog v-model="showAdd" title="添加上游账号" width="540px" destroy-on-close>
      <p class="dialog-hint">填入 OpenAI 兼容 API 地址。上游密钥加密后存入本地 SQLite。</p>
      <el-form label-position="top"><el-form-item label="账号名称"><el-input v-model="form.name" placeholder="例如：Gemini 免费 Key 1" /></el-form-item><el-form-item label="上游 Base URL"><el-input v-model="form.base_url" placeholder="https://api.example.com/v1" /></el-form-item><el-form-item label="上游 API Key"><el-input v-model="form.api_key" type="password" show-password /></el-form-item><div class="form-row"><el-form-item label="公开模型名"><el-input v-model="form.public_model" placeholder="fast-chat" /></el-form-item><el-form-item label="上游模型名"><el-input v-model="form.upstream_model" placeholder="provider-model-id" /></el-form-item></div><el-form-item label="顺序优先级"><el-input-number v-model="form.priority" :min="1" :max="9999" /><span class="form-note">数值越小，顺序模式越先尝试</span></el-form-item></el-form>
      <template #footer><el-button @click="showAdd=false">取消</el-button><el-button type="primary" :loading="saving" @click="addAccount">保存账号</el-button></template>
    </el-dialog>
  </div>
</template>

