<script setup lang="ts">
// Renders the Daffa API from the OpenAPI document the server generates from its route table.
// The spec is sourced fresh on every build (scripts/prebuild.mjs copies internal/api/openapi.json
// into public/), so this page can never drift from the routes. Each operation surfaces the
// capability it needs and where that check happens — the x-daffa-* extensions the generator emits.
import { ref, computed, onMounted } from 'vue'
import { withBase } from 'vitepress'

const METHODS = ['get', 'post', 'put', 'patch', 'delete'] as const

type AnySchema = Record<string, any>
interface Op {
  id: string; method: string; path: string; summary: string; description: string
  cap: string; scope: string; open: string; auth: string; isPublic: boolean
  params: { name: string; where: string; type: string; required: boolean; desc: string }[]
  bodyRequired: boolean
  bodyFields: { name: string; type: string; required: boolean; desc: string }[] | null
  responses: { code: string; desc: string }[]
}
interface Group { tag: string; desc: string; ops: Op[] }

const groups = ref<Group[]>([])
const error = ref('')
const loaded = ref(false)
const query = ref('')

const filtered = computed<Group[]>(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return groups.value
  return groups.value
    .map((g) => ({ ...g, ops: g.ops.filter((o) => (o.method + ' ' + o.path + ' ' + o.summary).toLowerCase().includes(q)) }))
    .filter((g) => g.ops.length)
})

const total = computed(() => groups.value.reduce((n, g) => n + g.ops.length, 0))

function refName(ref: string) { return String(ref).split('/').pop() as string }

onMounted(async () => {
  try {
    const res = await fetch(withBase('/openapi.json'), { cache: 'no-cache' })
    if (!res.ok) throw new Error('HTTP ' + res.status)
    build(await res.json())
  } catch (e: any) {
    error.value = e?.message || String(e)
  } finally {
    loaded.value = true
  }
})

function build(spec: AnySchema) {
  const schemas: Record<string, AnySchema> = (spec.components && spec.components.schemas) || {}
  const globalSecurity: AnySchema[] = spec.security || []

  const typeLabel = (schema: AnySchema | undefined): string => {
    if (!schema) return 'any'
    if (schema.$ref) return refName(schema.$ref)
    if (schema.enum) return 'enum'
    if (schema.type === 'array') return typeLabel(schema.items) + '[]'
    const combo = schema.allOf || schema.oneOf || schema.anyOf
    if (combo) return combo.map(typeLabel).join(schema.oneOf ? ' | ' : ' & ')
    if (schema.format) return `${schema.type} (${schema.format})`
    return schema.type || 'object'
  }
  const authNames = (op: AnySchema): { text: string; isPublic: boolean } => {
    const sec = op.security != null ? op.security : globalSecurity
    if (Array.isArray(sec) && sec.length === 0) return { text: '', isPublic: true }
    const names = new Set<string>()
    ;(sec || []).forEach((req: AnySchema) => Object.keys(req).forEach((k) => names.add(k)))
    return { text: [...names].join(' or '), isPublic: false }
  }
  const slug = (s: string) => String(s).replace(/[^a-z0-9]+/gi, '-').replace(/^-|-$/g, '').toLowerCase()

  const order: string[] = (spec.tags || []).map((t: AnySchema) => t.name)
  const descOf: Record<string, string> = {}
  ;(spec.tags || []).forEach((t: AnySchema) => (descOf[t.name] = t.description || ''))
  const byTag: Record<string, Op[]> = {}
  order.forEach((t) => (byTag[t] = []))

  Object.keys(spec.paths).forEach((path) => {
    const item = spec.paths[path]
    METHODS.forEach((m) => {
      const op = item[m]
      if (!op) return
      const tag = (op.tags && op.tags[0]) || 'other'
      if (!byTag[tag]) { byTag[tag] = []; order.push(tag) }

      const params = (op.parameters || []).map((p: AnySchema) => ({
        name: p.name, where: p.in, required: !!p.required,
        type: p.schema ? typeLabel(p.schema) : '', desc: p.description || '',
      }))

      let bodyFields: Op['bodyFields'] = null
      let bodyRequired = false
      const rb = op.requestBody
      if (rb && rb.content) {
        bodyRequired = !!rb.required
        const ct = rb.content['application/json'] || rb.content[Object.keys(rb.content)[0]]
        let schema = ct && ct.schema
        if (schema && schema.$ref) schema = schemas[refName(schema.$ref)] || schema
        if (schema && schema.properties) {
          const req: string[] = schema.required || []
          bodyFields = Object.keys(schema.properties).map((name) => {
            const ps = schema.properties[name]
            return {
              name, type: typeLabel(ps), required: req.includes(name),
              desc: ps.description || (ps.enum ? 'one of: ' + ps.enum.join(', ') : ''),
            }
          })
        } else {
          bodyFields = []
        }
      }

      const responses = Object.keys(op.responses || {}).sort().map((code) => ({
        code, desc: (op.responses[code] && op.responses[code].description) || '',
      }))

      const a = authNames(op)
      byTag[tag].push({
        id: 'op-' + (op.operationId ? slug(op.operationId) : slug(m + path)),
        method: m, path, summary: op.summary || '', description: op.description || '',
        cap: op['x-daffa-capability'] || '', scope: op['x-daffa-scope'] || '',
        open: op['x-daffa-open'] || '', auth: a.text, isPublic: a.isPublic,
        params, bodyRequired, bodyFields, responses,
      })
    })
  })

  groups.value = order.filter((t) => byTag[t] && byTag[t].length).map((t) => ({ tag: t, desc: descOf[t] || '', ops: byTag[t] }))
}
</script>

<template>
  <div class="apiref">
    <div v-if="error" class="api-empty">
      Could not load <code>openapi.json</code> ({{ error }}). It is generated by
      <code>go generate ./internal/api</code> and copied in by the docs build.
    </div>
    <div v-else-if="!loaded" class="api-empty">Loading the API…</div>
    <template v-else>
      <input v-model="query" class="api-search" type="search" :placeholder="`Filter ${total} operations…`" aria-label="Filter operations" />

      <section v-for="g in filtered" :key="g.tag" class="api-tag">
        <h2 :id="'tag-' + g.tag.replace(/[^a-z0-9]+/gi, '-').toLowerCase()">{{ g.tag }}</h2>
        <p v-if="g.desc" class="tag-desc">{{ g.desc }}</p>

        <details v-for="o in g.ops" :key="o.id" :id="o.id" class="op">
          <summary>
            <span class="method" :class="'m-' + o.method">{{ o.method }}</span>
            <span class="op-path">{{ o.path }}</span>
            <span v-if="o.summary" class="op-summary">{{ o.summary }}</span>
          </summary>
          <div class="op-body">
            <p v-if="o.description">{{ o.description }}</p>

            <div class="op-meta">
              <span v-if="o.cap" class="chip cap">cap: {{ o.cap }}</span>
              <span v-if="o.cap && o.scope" class="chip">scope: {{ o.scope }}</span>
              <span v-else-if="o.open" class="chip">no capability required</span>
              <span v-if="o.isPublic" class="chip auth">public</span>
              <span v-else-if="o.auth" class="chip auth">auth: {{ o.auth }}</span>
            </div>
            <p v-if="o.open" class="reason">{{ o.open }}</p>

            <div v-if="o.params.length" class="op-section">
              <h5>Parameters</h5>
              <div v-for="p in o.params" :key="p.name" class="row">
                <div class="row-name">
                  {{ p.name }}<span v-if="p.required" class="req" title="required">*</span>
                  <span class="row-type">{{ p.where }}<template v-if="p.type"> · {{ p.type }}</template></span>
                </div>
                <div class="row-desc">{{ p.desc }}</div>
              </div>
            </div>

            <div v-if="o.bodyFields" class="op-section">
              <h5>Request body<template v-if="o.bodyRequired"> (required)</template></h5>
              <template v-if="o.bodyFields.length">
                <div v-for="f in o.bodyFields" :key="f.name" class="row">
                  <div class="row-name">
                    {{ f.name }}<span v-if="f.required" class="req" title="required">*</span>
                    <span class="row-type">{{ f.type }}</span>
                  </div>
                  <div class="row-desc">{{ f.desc }}</div>
                </div>
              </template>
              <p v-else class="muted">JSON body.</p>
            </div>

            <div v-if="o.responses.length" class="op-section">
              <h5>Responses</h5>
              <div v-for="r in o.responses" :key="r.code" class="resp">
                <span class="resp-code" :class="'resp-' + r.code[0]">{{ r.code }}</span>
                <span class="row-desc">{{ r.desc }}</span>
              </div>
            </div>
          </div>
        </details>
      </section>

      <p v-if="!filtered.length" class="api-empty">No operations match “{{ query }}”.</p>
    </template>
  </div>
</template>

<style scoped>
.apiref { margin-top: 1.5rem; }
.api-empty { color: var(--text-muted); padding: 2rem 0; }
.api-search {
  width: 100%; padding: 0.6rem 0.85rem; margin-bottom: 1.5rem;
  font-family: var(--font-sans); font-size: 0.95rem;
  background: var(--surface-raised); border: 1px solid var(--border-strong);
  border-radius: var(--radius-control); color: var(--text);
}
.api-tag h2 {
  text-transform: capitalize; font-size: 1.4rem; letter-spacing: -0.02em;
  margin: 2.5rem 0 0.4rem; padding-top: 1rem; border-top: 1px solid var(--border);
}
.api-tag:first-of-type h2 { border-top: 0; padding-top: 0; margin-top: 0.5rem; }
.tag-desc { color: var(--text-muted); margin-bottom: 1rem; }

.op { border: 1px solid var(--border); border-radius: var(--radius-card); background: var(--surface-raised); margin-bottom: 0.75rem; overflow: hidden; scroll-margin-top: 5rem; }
.op > summary { display: flex; align-items: center; gap: 0.75rem; padding: 0.8rem 1rem; cursor: pointer; list-style: none; }
.op > summary::-webkit-details-marker { display: none; }
.op > summary:hover { background: var(--surface-sunken); }
.op[open] > summary { border-bottom: 1px solid var(--border); }
.op-path { font-family: var(--font-mono); font-size: 0.88rem; font-weight: 500; word-break: break-all; }
.op-summary { color: var(--text-muted); font-size: 0.85rem; margin-left: auto; text-align: right; }

.method {
  flex: none; font-family: var(--font-mono); font-weight: 600; font-size: 0.66rem;
  text-transform: uppercase; letter-spacing: 0.03em; padding: 0.12rem 0.4rem;
  border-radius: 0.35rem; border: 1px solid transparent; min-width: 3.3rem; text-align: center;
}
.m-get { color: var(--color-marine-600); background: color-mix(in oklch, var(--color-marine-500) 15%, transparent); border-color: color-mix(in oklch, var(--color-marine-500) 35%, transparent); }
.m-post { color: var(--success); background: var(--success-soft); border-color: color-mix(in oklch, var(--success) 35%, transparent); }
.m-put, .m-patch { color: var(--warn); background: var(--warn-soft); border-color: color-mix(in oklch, var(--warn) 35%, transparent); }
.m-delete { color: var(--danger); background: var(--danger-soft); border-color: color-mix(in oklch, var(--danger) 35%, transparent); }
:root[data-theme='dark'] .m-get { color: var(--color-marine-400); }
@media (prefers-color-scheme: dark) { :root:not([data-theme='light']) .m-get { color: var(--color-marine-400); } }

.op-body { padding: 1rem 1.15rem; display: flex; flex-direction: column; gap: 0.9rem; }
.op-body > p { color: var(--text-muted); font-size: 0.92rem; margin: 0; }
.reason { font-size: 0.85rem; }
.op-meta { display: flex; flex-wrap: wrap; gap: 0.4rem; }
.chip { font-family: var(--font-mono); font-size: 0.72rem; padding: 0.12rem 0.5rem; border-radius: 999px; background: var(--surface-sunken); border: 1px solid var(--border); color: var(--text-muted); }
.chip.cap { color: var(--accent-text); background: var(--accent-soft); border-color: color-mix(in oklch, var(--accent) 30%, transparent); }
.chip.auth { color: var(--warn); background: var(--warn-soft); border-color: color-mix(in oklch, var(--warn) 30%, transparent); }
.op-section h5 { font-size: 0.74rem; text-transform: uppercase; letter-spacing: 0.06em; color: var(--text-subtle); font-family: var(--font-mono); margin: 0 0 0.4rem; }
.row { display: grid; grid-template-columns: minmax(7rem, 13rem) 1fr; gap: 0.4rem 1rem; padding: 0.4rem 0; border-top: 1px solid var(--border); font-size: 0.87rem; }
.row:first-of-type { border-top: 0; }
.row-name { font-family: var(--font-mono); color: var(--text); }
.req { color: var(--danger); margin-left: 0.15rem; }
.row-type { display: block; font-family: var(--font-mono); font-size: 0.76rem; color: var(--info); margin-top: 0.1rem; }
.row-desc { color: var(--text-muted); }
.muted { color: var(--text-muted); font-size: 0.85rem; }
.resp { display: flex; gap: 0.75rem; align-items: baseline; padding: 0.3rem 0; border-top: 1px solid var(--border); font-size: 0.87rem; }
.resp:first-of-type { border-top: 0; }
.resp-code { font-family: var(--font-mono); font-weight: 600; }
.resp-2 { color: var(--success); } .resp-4 { color: var(--warn); } .resp-5 { color: var(--danger); }

@media (max-width: 640px) {
  .op > summary { flex-wrap: wrap; }
  .op-summary { margin-left: 0; width: 100%; text-align: left; }
  .row { grid-template-columns: 1fr; }
}
</style>
