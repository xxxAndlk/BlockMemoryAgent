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
	rtNoCfg, err := rtPkg.New(soulPath(t), nil)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	if planEnabledFromRT(rtNoCfg) {
		t.Fatal("无 AgentCfg 时应返回 false")
	}
	rtDisabled, err := rtPkg.New(soulPath(t), nil)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rtDisabled.SetAgentConfig(&config.AgentConfig{FeatureTogglesConfig: config.FeatureTogglesConfig{PlanEnabled: false}})
	if planEnabledFromRT(rtDisabled) {
		t.Fatal("PlanEnabled=false 时应返回 false")
	}
	rtEnabled, err := rtPkg.New(soulPath(t), nil)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rtEnabled.SetAgentConfig(&config.AgentConfig{FeatureTogglesConfig: config.FeatureTogglesConfig{PlanEnabled: true}})
	if !planEnabledFromRT(rtEnabled) {
		t.Fatal("PlanEnabled=true 时应返回 true")
	}
}

func TestReflectionEnabledFromRT(t *testing.T) {
	if reflectionEnabledFromRT(nil) {
		t.Fatal("nil runtime 应返回 false")
	}
	rtDisabled, err := rtPkg.New(soulPath(t), nil)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rtDisabled.SetAgentConfig(&config.AgentConfig{FeatureTogglesConfig: config.FeatureTogglesConfig{ReflectionEnabled: false}})
	if reflectionEnabledFromRT(rtDisabled) {
		t.Fatal("ReflectionEnabled=false 时应返回 false")
	}
	rtEnabled, err := rtPkg.New(soulPath(t), nil)
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rtEnabled.SetAgentConfig(&config.AgentConfig{FeatureTogglesConfig: config.FeatureTogglesConfig{ReflectionEnabled: true}})
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
