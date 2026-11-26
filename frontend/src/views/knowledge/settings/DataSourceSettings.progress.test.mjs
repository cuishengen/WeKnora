import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { stripTypeScriptTypes } from 'node:module'
import { runInNewContext } from 'node:vm'
import test from 'node:test'

// Execute the component's actual setup script without mounting its unrelated
// editor and API controls. No frontend dependency install is needed for this test.
const source = readFileSync(new URL('./DataSourceSettings.vue', import.meta.url), 'utf8')
const setup = source.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
const script = stripTypeScriptTypes(setup).replace(/^import[\s\S]*?from\s+['"][^'"]+['"];?\s*$/gm, '')
const messages = {}
runInNewContext(readFileSync(new URL('../../../i18n/locales/zh-CN.ts', import.meta.url), 'utf8')
  .replace('export default', 'messages.value ='), { messages })
const t = (key, params = {}) => {
  const message = key.split('.').reduce((value, part) => value?.[part], messages.value)
  assert.equal(typeof message, 'string', `missing translation: ${key}`)
  return message.replace(/\{(\w+)\}/g, (_, name) => String(params[name] ?? ''))
}
const context = {
  defineProps: () => ({ kbId: 'vendor' }), defineEmits: () => () => {},
  useI18n: () => ({ t }), useAuthStore: () => ({ hasRole: () => false }),
  ref: value => ({ value }), computed: getter => ({ get value() { return getter() } }),
  onMounted() {}, onBeforeUnmount() {}, useAssistantDataChange() {},
  useConnectorLabel: () => ({ connectorLabel: () => '' }),
}
runInNewContext(script + '\nthis.result = { sourceProgressNotes, lastSyncStatusLabel }', context)
const { sourceProgressNotes, lastSyncStatusLabel } = context.result

test('a completed scan distinguishes pending recognition and deliberately deferred images', () => {
  const ds = {
    latest_sync_log: { status: 'success' },
    source_progress: { total: 7414, completed: 4386, deferred: 1, excluded: 3027,
      excluded_images: 3027, scan_complete: true },
  }
  assert.equal(sourceProgressNotes(ds), '扫描已完成 · 1 项待识别 · 3027 张图片按设置暂缓')
  assert.equal(lastSyncStatusLabel(ds), '同步完成（有待识别资料）')
  assert.equal(ds.source_progress.total, 7414)
})

test('a real failure remains partial even when other documents await recognition', () => {
  const ds = { latest_sync_log: { status: 'partial' }, source_progress: { deferred: 1, failed: 1 } }
  assert.equal(lastSyncStatusLabel(ds), t('datasource.logStatus.partial'))
  assert.equal(sourceProgressNotes(ds), '1 项待识别')
})

test('a new running scan does not reuse the previous completion announcement', () => {
  const ds = { latest_sync_log: { status: 'running' }, source_progress: { scan_complete: true, deferred: 1 } }
  assert.equal(sourceProgressNotes(ds), '1 项待识别')
  assert.equal(lastSyncStatusLabel(ds), t('datasource.logStatus.running'))
})

test('legacy and other connectors retain their status without inventing progress', () => {
  const ds = { type: 'github', latest_sync_log: { status: 'success' } }
  assert.equal(sourceProgressNotes(ds), '')
  assert.equal(lastSyncStatusLabel(ds), t('datasource.logStatus.success'))
  assert.equal(sourceProgressNotes({ source_progress: { scan_complete: false, excluded: 2 } }), '2 项按规则排除')
})
