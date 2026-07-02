package graph

import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/config"
	rtPkg "github.com/blockmemory/agent/backend/internal/runtime"
)

func TestPlanEnabledFromRT(t *testing.T) {
	if planEnabledFromRT(nil) {
		t.Fatal("nil runtime 应返回 false")
	}
	rtNoCfg := rtPkg.New(soulPath(t), nil)
	if planEnabledFromRT(rtNoCfg) {
		t.Fatal("无 AgentCfg 时应返回 false")
	}
	rtDisabled := rtPkg.New(soulPath(t), nil)
	rtDisabled.SetAgentConfig(&config.AgentConfig{PlanEnabled: false})
	if planEnabledFromRT(rtDisabled) {
		t.Fatal("PlanEnabled=false 时应返回 false")
	}
	rtEnabled := rtPkg.New(soulPath(t), nil)
	rtEnabled.SetAgentConfig(&config.AgentConfig{PlanEnabled: true})
	if !planEnabledFromRT(rtEnabled) {
		t.Fatal("PlanEnabled=true 时应返回 true")
	}
}

func TestReflectionEnabledFromRT(t *testing.T) {
	if reflectionEnabledFromRT(nil) {
		t.Fatal("nil runtime 应返回 false")
	}
	rtDisabled := rtPkg.New(soulPath(t), nil)
	rtDisabled.SetAgentConfig(&config.AgentConfig{ReflectionEnabled: false})
	if reflectionEnabledFromRT(rtDisabled) {
		t.Fatal("ReflectionEnabled=false 时应返回 false")
	}
	rtEnabled := rtPkg.New(soulPath(t), nil)
	rtEnabled.SetAgentConfig(&config.AgentConfig{ReflectionEnabled: true})
	if !reflectionEnabledFromRT(rtEnabled) {
		t.Fatal("ReflectionEnabled=true 时应返回 true")
	}
}

func TestReflectOnResultWithNoModelFactory(t *testing.T) {
	// 无 modelFactory 时 reflectOnResult 应视为通过，不重试
	ok, feedback := reflectOnResult(nil, nil, "任务", "结果")
	if !ok || feedback != "" {
		t.Fatalf("无 modelFactory 时应返回 (true,\"\"), got (%v,%s)", ok, feedback)
	}
}
