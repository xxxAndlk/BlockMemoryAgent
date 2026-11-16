//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveDepTargets_FromEnv 校验地址解析：DSN 取主机/端口、Redis 补齐端口、
// 本机判定（决定要不要拉本地 docker 起依赖）。
func TestResolveDepTargets_FromEnv(t *testing.T) {
	dir := t.TempDir()
	env := "POSTGRES_DSN=postgres://u:p@127.0.0.1:5433/db?sslmode=disable\n" +
		"REDIS_ADDR=localhost:6390\n" +
		"REDIS_PASSWORD=pw\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
		t.Fatalf("写 .env 失败: %v", err)
	}

	tg := resolveDepTargets(dir)
	if tg.pgHost != "127.0.0.1" || tg.pgPort != "5433" {
		t.Errorf("PG 目标解析错误: host=%q port=%q", tg.pgHost, tg.pgPort)
	}
	if tg.redisAddr != "localhost:6390" {
		t.Errorf("Redis 地址解析错误: %q", tg.redisAddr)
	}
	if tg.redisPass != "pw" {
		t.Errorf("Redis 口令解析错误: %q", tg.redisPass)
	}
	if !tg.pgLocal || !tg.redisLocal {
		t.Errorf("本机判定错误: pgLocal=%v redisLocal=%v", tg.pgLocal, tg.redisLocal)
	}
}

// TestResolveDepTargets_Remote 远端依赖不算本机（不该去起本地 docker）。
func TestResolveDepTargets_Remote(t *testing.T) {
	dir := t.TempDir()
	env := "POSTGRES_DSN=postgres://u:p@db.internal:5432/db\nREDIS_ADDR=cache.internal:6379\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o644); err != nil {
		t.Fatalf("写 .env 失败: %v", err)
	}

	tg := resolveDepTargets(dir)
	if tg.pgLocal || tg.redisLocal {
		t.Errorf("远端依赖被误判为本机: pgLocal=%v redisLocal=%v", tg.pgLocal, tg.redisLocal)
	}
}

// TestResolveDepTargets_RedisNoPort Redis 地址缺端口时补默认 6379。
func TestResolveDepTargets_RedisNoPort(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("REDIS_ADDR=localhost\n"), 0o644); err != nil {
		t.Fatalf("写 .env 失败: %v", err)
	}

	tg := resolveDepTargets(dir)
	if tg.redisAddr != "localhost:6379" {
		t.Errorf("Redis 默认端口错误: %q", tg.redisAddr)
	}
}

// TestTailLines 错误摘要只取末尾若干行。
func TestTailLines(t *testing.T) {
	got := tailLines("a\nb\nc\nd", 2)
	if got != "c | d" {
		t.Errorf("tailLines = %q", got)
	}
	if tailLines("", 3) != "" {
		t.Error("空输入应返回空串")
	}
}
