<template>
  <el-config-provider>
    <router-view></router-view>
  </el-config-provider>
</template>

<script setup lang="ts">
// 主题由 useTheme 统一管理（localStorage bma:theme，默认浅色）；
// 此处不再强制挂 dark class——曾导致默认进暗色且主题图标状态错反。
</script>

<style>
#app {
  height: 100vh;
  width: 100vw;
  overflow: hidden;
}
</style>
