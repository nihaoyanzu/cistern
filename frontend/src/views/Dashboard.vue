<script setup lang="ts">
import PageHeader from '@/components/PageHeader.vue'
import DashboardOverview from '@/components/DashboardOverview.vue'
import { useStatsStore } from '@/stores/stats'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()
const statsStore = useStatsStore()
const stats = computed(() => statsStore.data)
</script>

<template>
  <div>
    <PageHeader :title="t('dashboard.title')" />
    <DashboardOverview :stats="stats" />
  </div>
</template>
