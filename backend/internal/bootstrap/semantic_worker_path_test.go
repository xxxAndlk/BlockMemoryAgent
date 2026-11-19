package bootstrap

// semantic_worker_path_test.go 覆盖 2026-11-19 再次验收 §6.2：
// 桌面包必须真的带上语义编辑 worker，并且后端能按安装布局解析到它。
//
// 这里锁的是"路径解析"这一环（可单测）；打包本身由 bma-app.ps1 pack 负责，
// 用脚本级断言（见 doc/Harness根治_再次验收整改记录_2026-11-19.md）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// TestResolveSemanticWorkerPathPrecedence 显式配置 > 环境变量（桌面注入）> 仓库默认。
func TestResolveSemanticWorkerPathPrecedence(t *testing.T) {
	const envKey = "BMA_SEMANTIC_EDIT_WORKER"
	orig, had := os.LookupEnv(envKey)
	defer func() {
		if had {
			_ = os.Setenv(envKey, orig)
		} else {
			_ = os.Unsetenv(envKey)
		}
	}()

	// 1) 显式配置最优先（环境变量存在也不影响）。
	_ = os.Setenv(envKey, filepath.Join("runtime", "media", "semantic-edit", "worker.mjs"))
	if got, want := resolveSemanticWorkerPath("  config/worker.mjs "), "config/worker.mjs"; got != want {
		t.Fatalf("显式配置必须优先：got %q want %q", got, want)
	}

	// 2) 未配置时取环境变量（桌面外壳/启动器注入的安装目录绝对路径）。
	envPath := filepath.Join("C:", "Apps", "BMA", "runtime", "media", "semantic-edit", "worker.mjs")
	_ = os.Setenv(envKey, envPath)
	if got := resolveSemanticWorkerPath(""); got != envPath {
		t.Fatalf("环境变量必须生效：got %q want %q", got, envPath)
	}

	// 3) 都没有 → 仓库开发布局。
	_ = os.Unsetenv(envKey)
	got := resolveSemanticWorkerPath("")
	if !strings.HasSuffix(got, filepath.Join("tools", "semantic-edit", "worker.mjs")) {
		t.Fatalf("默认应为仓库开发布局，got %q", got)
	}
}

// TestConfigureSemanticEditOffIsNoop off 模式不装配（零行为）。
func TestConfigureSemanticEditOffIsNoop(t *testing.T) {
	for _, mode := range []string{"", "off", "garbage"} {
		reg := newTestRegistry(t)
		configureSemanticEdit(reg, mode, "")
		// off 下 EditFile 不要求语义参数（Schema/执行侧都不介入）。这里只断言模式归一。
		if got := reg.SemanticEditMode(); got != "off" {
			t.Fatalf("mode=%q 应保持 off，got %q", mode, got)
		}
	}
}

// TestConfigureSemanticEditEnforceInstallsWorker enforce 会装配 worker（即使脚本缺失，
// 也只是让每次编辑 fail closed，而不是静默关闭能力）。
func TestConfigureSemanticEditEnforceInstallsWorker(t *testing.T) {
	reg := newTestRegistry(t)
	configureSemanticEdit(reg, "enforce", filepath.Join(t.TempDir(), "missing-worker.mjs"))
	if got := reg.SemanticEditMode(); got != "enforce" {
		t.Fatalf("enforce 必须装配（fail closed 而非关闭能力），got %q", got)
	}
}

// newTestRegistry 构造最小 Registry（只需 exec 存在，供 SetSemanticEdit/SemanticEditMode）。
func newTestRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
}
