/** 前端模块 UI 描述符：加平台时优先改此表，少改 Platform/Guide/AppShell 分支。 */

export type PrefetchUI = 'none' | 'hint' | 'arch' | 'pypiWheel'

export type ThirdField = 'none' | 'metadata' | 'token'

export type CatalogMode = 'pypi' | 'local'

export interface ModuleDescriptor {
  id: string
  labelKey: string
  /** 公开说明页 Tab 标题（品牌短名，与语言无关） */
  guideTitle: string
  enableLabelKey: string
  showPrefetchTab: boolean
  /** 公开说明页 API 失败时的兜底 enabled */
  guideDefaultEnabled: boolean
  /** 包检索页行为：pypi=目录检索；local=本地缓存浏览 */
  catalogMode: CatalogMode
  /** CSS 变量名，如 --color-module-pypi */
  colorVar: string
  /** 侧栏模块符号（AppShell nav-item-icon） */
  navIcon: string
  nav: {
    catalog: boolean
    prefetch: boolean
    catalogLabelKey: string
    prefetchLabelKey: string
  }
  defaults: {
    upstream: string
    file_upstream: string
    metadata_upstream?: string
  }
  fields: {
    thirdField: ThirdField
    persistUpstreamToken: boolean
    /** true：界面只编一个上游，保存时 file_upstream 与 upstream 同步 */
    unifiedUpstream?: boolean
    /**
     * thirdField=none 且 unified 时，是否把 metadata_upstream 写成与 upstream 相同。
     * Go/npm 等共用源为 true；Docker Auth 由 /v2/ 自动发现，应为 false（保存空串）。
     */
    syncMetadataWithUpstream?: boolean
  }
  labels: {
    upstreamKey: string
    fileUpstreamKey: string
    thirdFieldKey?: string
  }
  hints: {
    upstreamHintKey?: string
    prefetchHintKey?: string
    /** 预拉取页粘贴区说明 */
    prefetchPasteHintKey?: string
    /** 预拉取页 textarea 占位示例 */
    prefetchPlaceholderKey?: string
    thirdFieldHintKey?: string
    thirdFieldPlaceholder?: string
  }
  prefetchUI: PrefetchUI
}

export const MODULES: ModuleDescriptor[] = [
  {
    id: 'pypi',
    labelKey: 'platform.modulePyPI',
    guideTitle: 'PyPI',
    enableLabelKey: 'platform.enablePyPI',
    showPrefetchTab: true,
    guideDefaultEnabled: true,
    catalogMode: 'pypi',
    colorVar: '--color-module-pypi',
    navIcon: 'pypi',
    nav: {
      catalog: true,
      prefetch: true,
      catalogLabelKey: 'nav.pkgSearch',
      prefetchLabelKey: 'nav.prefetch',
    },
    defaults: {
      upstream: 'https://mirrors.aliyun.com/pypi',
      file_upstream: 'https://mirrors.aliyun.com/pypi',
      metadata_upstream: '',
    },
    fields: {
      thirdField: 'metadata',
      persistUpstreamToken: false,
      unifiedUpstream: true,
    },
    labels: {
      upstreamKey: 'platform.pypiUpstream',
      fileUpstreamKey: 'platform.fileUpstream',
      thirdFieldKey: 'platform.metadataUpstream',
    },
    hints: {
      upstreamHintKey: 'platform.pypiUpstreamHint',
      thirdFieldHintKey: 'platform.metadataHint',
      thirdFieldPlaceholder: 'PEP 658；留空回退到上游',
      prefetchHintKey: 'platform.pypiPrefetchHint',
      prefetchPasteHintKey: 'prefetch.hintPypi',
      prefetchPlaceholderKey: 'prefetch.placeholderPypi',
    },
    prefetchUI: 'pypiWheel',
  },
  {
    id: 'npm',
    labelKey: 'platform.moduleNpm',
    guideTitle: 'npm',
    enableLabelKey: 'platform.enableNpm',
    showPrefetchTab: true,
    guideDefaultEnabled: true,
    catalogMode: 'local',
    colorVar: '--color-module-npm',
    navIcon: 'npm',
    nav: {
      catalog: true,
      prefetch: true,
      catalogLabelKey: 'nav.localCache',
      prefetchLabelKey: 'nav.prefetch',
    },
    defaults: {
      upstream: 'https://registry.npmmirror.com',
      file_upstream: 'https://registry.npmmirror.com',
      metadata_upstream: '',
    },
    fields: {
      thirdField: 'none',
      persistUpstreamToken: false,
      unifiedUpstream: true,
    },
    labels: {
      upstreamKey: 'platform.registryUpstream',
      fileUpstreamKey: 'platform.fileUpstream',
    },
    hints: {
      upstreamHintKey: 'platform.npmUpstreamHint',
      prefetchHintKey: 'platform.npmPrefetchHint',
      prefetchPasteHintKey: 'prefetch.hintNpm',
      prefetchPlaceholderKey: 'prefetch.placeholderNpm',
    },
    prefetchUI: 'none',
  },
  {
    id: 'docker',
    labelKey: 'platform.moduleDocker',
    guideTitle: 'Docker',
    enableLabelKey: 'platform.enableDocker',
    showPrefetchTab: true,
    guideDefaultEnabled: true,
    catalogMode: 'local',
    colorVar: '--color-module-docker',
    navIcon: 'docker',
    nav: {
      catalog: true,
      prefetch: true,
      catalogLabelKey: 'nav.localCache',
      prefetchLabelKey: 'nav.prefetch',
    },
    defaults: {
      // 与后端首次安装种子一致（国内镜像）；表单留空保存后由后端回落官方源
      // Auth 由 registry /v2/ WWW-Authenticate 自动发现，无需再配 metadata_upstream
      upstream: 'https://docker.m.daocloud.io',
      file_upstream: 'https://docker.m.daocloud.io',
      metadata_upstream: '',
    },
    fields: {
      thirdField: 'none',
      persistUpstreamToken: false,
      unifiedUpstream: true,
      syncMetadataWithUpstream: false,
    },
    labels: {
      upstreamKey: 'platform.registryUpstream',
      fileUpstreamKey: 'platform.blobUpstream',
    },
    hints: {
      upstreamHintKey: 'platform.dockerUpstreamHint',
      prefetchHintKey: 'platform.dockerPrefetchHint',
      prefetchPasteHintKey: 'prefetch.hintDocker',
      prefetchPlaceholderKey: 'prefetch.placeholderDocker',
    },
    prefetchUI: 'arch',
  },
  {
    id: 'goproxy',
    labelKey: 'platform.moduleGoproxy',
    guideTitle: 'Go',
    enableLabelKey: 'platform.enableGoproxy',
    showPrefetchTab: true,
    guideDefaultEnabled: true,
    catalogMode: 'local',
    colorVar: '--color-module-goproxy',
    navIcon: 'goproxy',
    nav: {
      catalog: true,
      prefetch: true,
      catalogLabelKey: 'nav.localCache',
      prefetchLabelKey: 'nav.prefetch',
    },
    defaults: {
      upstream: 'https://goproxy.cn',
      file_upstream: 'https://goproxy.cn',
      metadata_upstream: '',
    },
    fields: {
      thirdField: 'none',
      persistUpstreamToken: false,
      unifiedUpstream: true,
    },
    labels: {
      upstreamKey: 'platform.moduleUpstream',
      fileUpstreamKey: 'platform.moduleFileUpstream',
    },
    hints: {
      upstreamHintKey: 'platform.goproxyUpstreamHint',
      prefetchHintKey: 'platform.goproxyPrefetchHint',
      prefetchPasteHintKey: 'prefetch.hintGoproxy',
      prefetchPlaceholderKey: 'prefetch.placeholderGoproxy',
    },
    prefetchUI: 'hint',
  },
  {
    id: 'huggingface',
    labelKey: 'platform.moduleHuggingFace',
    guideTitle: 'Hugging Face',
    enableLabelKey: 'platform.enableHuggingFace',
    showPrefetchTab: true,
    guideDefaultEnabled: true,
    catalogMode: 'local',
    colorVar: '--color-module-huggingface',
    navIcon: 'huggingface',
    nav: {
      catalog: true,
      prefetch: true,
      catalogLabelKey: 'nav.localCache',
      prefetchLabelKey: 'nav.prefetch',
    },
    defaults: {
      upstream: 'https://hf-mirror.com',
      file_upstream: 'https://hf-mirror.com',
      metadata_upstream: '',
    },
    fields: {
      thirdField: 'token',
      persistUpstreamToken: true,
      unifiedUpstream: true,
    },
    labels: {
      upstreamKey: 'platform.hubUpstream',
      fileUpstreamKey: 'platform.hubFileUpstream',
      thirdFieldKey: 'platform.upstreamToken',
    },
    hints: {
      upstreamHintKey: 'platform.huggingfaceUpstreamHint',
      prefetchHintKey: 'platform.huggingfacePrefetchHint',
      thirdFieldHintKey: 'platform.upstreamTokenHint',
      prefetchPasteHintKey: 'prefetch.hintHuggingFace',
      prefetchPlaceholderKey: 'prefetch.placeholderHuggingFace',
    },
    prefetchUI: 'hint',
  },
  {
    id: 'maven',
    labelKey: 'platform.moduleMaven',
    guideTitle: 'Maven',
    enableLabelKey: 'platform.enableMaven',
    showPrefetchTab: true,
    guideDefaultEnabled: false,
    catalogMode: 'local',
    colorVar: '--color-module-maven',
    navIcon: 'maven',
    nav: {
      catalog: true,
      prefetch: true,
      catalogLabelKey: 'nav.localCache',
      prefetchLabelKey: 'nav.prefetch',
    },
    defaults: {
      upstream: 'https://maven.aliyun.com/repository/central',
      file_upstream: 'https://maven.aliyun.com/repository/central',
      metadata_upstream: '',
    },
    fields: {
      thirdField: 'none',
      persistUpstreamToken: false,
      unifiedUpstream: true,
    },
    labels: {
      upstreamKey: 'platform.mavenRepoUpstream',
      fileUpstreamKey: 'platform.mavenFileUpstream',
    },
    hints: {
      upstreamHintKey: 'platform.mavenUpstreamHint',
      prefetchHintKey: 'platform.mavenPrefetchHint',
      prefetchPasteHintKey: 'prefetch.hintMaven',
      prefetchPlaceholderKey: 'prefetch.placeholderMaven',
    },
    prefetchUI: 'hint',
  },
]

export const MODULE_BY_ID: Record<string, ModuleDescriptor> = Object.fromEntries(
  MODULES.map((d) => [d.id, d]),
)

/** 模块色 CSS 值，供图表/徽章使用 */
export function moduleColor(id: string): string {
  const d = MODULE_BY_ID[id]
  if (d?.colorVar) return `var(${d.colorVar})`
  return 'var(--color-muted)'
}

export function isKnownModule(id: string): boolean {
  return id in MODULE_BY_ID
}

export function guideFallbackModules(): { id: string; enabled: boolean }[] {
  return MODULES.map((d) => ({ id: d.id, enabled: d.guideDefaultEnabled }))
}

/** 按侧栏 MODULES 顺序排列（API 注册序可能不同）；未知 id 追加末尾。 */
export function orderModulesByNav<T extends { id: string }>(list: T[]): T[] {
  const byId = new Map(list.map((m) => [m.id, m]))
  const ordered: T[] = []
  for (const d of MODULES) {
    const m = byId.get(d.id)
    if (m) {
      ordered.push(m)
      byId.delete(d.id)
    }
  }
  for (const m of byId.values()) ordered.push(m)
  return ordered
}

/** 侧栏/路由：具备包检索二级入口的模块 */
export function modulesWithCatalogNav(enabled: Record<string, boolean>): ModuleDescriptor[] {
  return MODULES.filter((d) => d.nav.catalog && enabled[d.id])
}

/** 侧栏/路由：具备预拉取二级入口的模块 */
export function modulesWithPrefetchNav(enabled: Record<string, boolean>): ModuleDescriptor[] {
  return MODULES.filter((d) => d.nav.prefetch && enabled[d.id])
}
