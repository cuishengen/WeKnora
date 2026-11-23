<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { getKnowledgeDetails, downKnowledgeDetails } from '@/api/knowledge-base'

const route = useRoute()
const message = ref('正在准备原件下载…')
const busy = ref(false)
const download = async () => {
  busy.value = true
  try {
    const response: any = await getKnowledgeDetails(String(route.params.id))
    const knowledge = response.data
    const blob = await downKnowledgeDetails(String(route.params.id))
    const link = document.createElement('a')
    const url = URL.createObjectURL(blob)
    link.href = url
    link.download = knowledge.source_reference?.relative_path?.split('/').pop() || knowledge.file_name || knowledge.title
    document.body.appendChild(link)
    link.click()
    link.remove()
    setTimeout(() => URL.revokeObjectURL(url), 60000)
    message.value = '原件已开始下载。此链接沿用你的账号权限，可复制给有相应权限的同事。'
  } catch (error: any) {
    message.value = error?.message || '无法下载，请确认资料权限或原件是否已更新。'
  } finally { busy.value = false }
}
onMounted(download)
</script>

<template>
  <div style="padding:32px">
    <h2>原件下载</h2>
    <p>{{ message }}</p>
    <t-button :loading="busy" @click="download">重新下载</t-button>
  </div>
</template>
