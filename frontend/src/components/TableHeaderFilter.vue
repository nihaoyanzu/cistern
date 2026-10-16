<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'

/** 表头下拉过滤：自定义面板（原生 select 下拉无法美化），供各表格复用。 */
export type TableFilterOption = { value: string; label: string }

const model = defineModel<string>({ default: '' })

const props = withDefaults(
  defineProps<{
    options: TableFilterOption[]
    /** 空值选项文案；传入则渲染 value="" 的「全部」项 */
    allLabel?: string
    ariaLabel?: string
  }>(),
  {
    allLabel: '',
    ariaLabel: '',
  },
)

const open = ref(false)
const rootRef = ref<HTMLElement | null>(null)
const triggerRef = ref<HTMLButtonElement | null>(null)
const menuRef = ref<HTMLElement | null>(null)
const menuStyle = ref<Record<string, string>>({})

const items = computed(() => {
  const list: TableFilterOption[] = []
  if (props.allLabel) list.push({ value: '', label: props.allLabel })
  list.push(...props.options)
  return list
})

const active = computed(() => model.value !== '')

/** 未筛选时显示列名（优先级/状态等）；「全部」仅出现在下拉项里 */
const displayLabel = computed(() => {
  if (model.value === '') return props.ariaLabel || props.allLabel || '—'
  const hit = items.value.find((o) => o.value === model.value)
  return hit?.label || props.ariaLabel || '—'
})

function placeMenu() {
  const el = triggerRef.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  const width = Math.max(rect.width, 8.5 * 16)
  let left = rect.left
  const maxLeft = window.innerWidth - width - 8
  if (left > maxLeft) left = Math.max(8, maxLeft)
  const below = rect.bottom + 6
  const spaceBelow = window.innerHeight - below
  const preferUp = spaceBelow < 220 && rect.top > spaceBelow
  menuStyle.value = preferUp
    ? {
        left: `${left}px`,
        bottom: `${window.innerHeight - rect.top + 6}px`,
        top: 'auto',
        minWidth: `${width}px`,
      }
    : {
        left: `${left}px`,
        top: `${below}px`,
        bottom: 'auto',
        minWidth: `${width}px`,
      }
}

async function openMenu() {
  open.value = true
  await nextTick()
  placeMenu()
}

function closeMenu() {
  open.value = false
}

function toggleMenu() {
  if (open.value) closeMenu()
  else void openMenu()
}

function select(value: string) {
  model.value = value
  closeMenu()
}

function onDocPointer(e: MouseEvent) {
  if (!open.value) return
  const t = e.target as Node
  if (rootRef.value?.contains(t) || menuRef.value?.contains(t)) return
  closeMenu()
}

function onKey(e: KeyboardEvent) {
  if (!open.value) return
  if (e.key === 'Escape') {
    e.preventDefault()
    closeMenu()
    triggerRef.value?.focus()
  }
}

function onReposition() {
  if (open.value) placeMenu()
}

watch(open, (v) => {
  if (v) placeMenu()
})

onMounted(() => {
  document.addEventListener('mousedown', onDocPointer)
  document.addEventListener('keydown', onKey)
  window.addEventListener('resize', onReposition)
  window.addEventListener('scroll', onReposition, true)
})
onUnmounted(() => {
  document.removeEventListener('mousedown', onDocPointer)
  document.removeEventListener('keydown', onKey)
  window.removeEventListener('resize', onReposition)
  window.removeEventListener('scroll', onReposition, true)
})
</script>

<template>
  <div ref="rootRef" class="ui-table-filter-wrap">
    <button
      ref="triggerRef"
      type="button"
      class="ui-table-filter"
      :class="{ 'is-active': active, 'is-open': open }"
      :aria-label="ariaLabel || undefined"
      :aria-expanded="open"
      aria-haspopup="listbox"
      @click.stop="toggleMenu"
    >
      <span class="ui-table-filter-label">{{ displayLabel }}</span>
      <svg
        class="ui-table-filter-chevron"
        :class="{ 'is-open': open }"
        viewBox="0 0 20 20"
        fill="currentColor"
        aria-hidden="true"
      >
        <path
          fill-rule="evenodd"
          d="M5.293 7.293a1 1 0 011.414 0L10 10.586l3.293-3.293a1 1 0 111.414 1.414l-4 4a1 1 0 01-1.414 0l-4-4a1 1 0 010-1.414z"
          clip-rule="evenodd"
        />
      </svg>
    </button>

    <Teleport to="body">
      <div
        v-show="open"
        ref="menuRef"
        class="ui-table-filter-menu"
        role="listbox"
        :aria-label="ariaLabel || undefined"
        :style="menuStyle"
        @mousedown.stop
      >
        <button
          v-for="opt in items"
          :key="opt.value === '' ? '__all__' : opt.value"
          type="button"
          class="ui-table-filter-option"
          :class="{ 'is-selected': model === opt.value }"
          role="option"
          :aria-selected="model === opt.value"
          @click="select(opt.value)"
        >
          <span class="ui-table-filter-option-label">{{ opt.label }}</span>
          <svg
            v-if="model === opt.value"
            class="ui-table-filter-check"
            viewBox="0 0 20 20"
            fill="currentColor"
            aria-hidden="true"
          >
            <path
              fill-rule="evenodd"
              d="M16.707 5.293a1 1 0 010 1.414l-8 8a1 1 0 01-1.414 0l-4-4a1 1 0 011.414-1.414L8 12.586l7.293-7.293a1 1 0 011.414 0z"
              clip-rule="evenodd"
            />
          </svg>
        </button>
      </div>
    </Teleport>
  </div>
</template>
