<template>
  <div class="h-full overflow-y-auto text-ink">
    <div class="max-w-2xl mx-auto flex flex-col items-center text-center pt-16">
      <div class="rounded-full bg-primary-soft p-5 mb-4">
        <el-icon class="text-5xl text-primary"><SetUp /></el-icon>
      </div>
      <div class="flex items-center gap-2 mb-1">
        <span class="text-lg font-bold">工作流编排</span>
        <el-tag size="small" type="warning" effect="plain">Beta</el-tag>
      </div>
      <div class="text-sm text-ink-2 mb-6">可视化编排多 Agent 工作流，该能力为 Beta 预留，入口先行开放。</div>

      <div class="w-full bg-card border border-line rounded-card p-5 text-left text-sm space-y-3">
        <div class="font-bold text-sm">规划能力</div>
        <div class="flex items-start gap-2 text-ink-2"><el-icon class="mt-0.5 text-primary"><Connection /></el-icon>拖拽节点编排：Meta / Domain / Assistant 角色自由组合</div>
        <div class="flex items-start gap-2 text-ink-2"><el-icon class="mt-0.5 text-primary"><Share /></el-icon>依赖连线与条件分支，声明式 DAG</div>
        <div class="flex items-start gap-2 text-ink-2"><el-icon class="mt-0.5 text-primary"><Monitor /></el-icon>运行观测：节点级状态、日志与指标联动会话监控</div>
      </div>
    </div>
  </div>
</template>
