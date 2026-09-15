package store

// domain_profile_store_test.go 领域档案存储与按源删除原语的 nil-DB 降级测试
//（TODO #17 T23 / #16 T21）：零真实 PG，验证 nil-DB 静默返回不 panic。
import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

func TestDomainProfileStoreNilDBNoop(t *testing.T) {
	var s *KnowledgeStore
	ctx := context.Background()

	// UpsertDomainProfile：nil 接收者静默返回。
	if err := s.UpsertDomainProfile(ctx, DomainProfileUpsert{Domain: "前端工程师", Files: []string{"a.go"}}); err != nil {
		t.Fatalf("nil-DB UpsertDomainProfile = %v, want nil", err)
	}
	// 实例存在但 db 未接线：同样静默。
	s2 := &KnowledgeStore{}
	if err := s2.UpsertDomainProfile(ctx, DomainProfileUpsert{Domain: "前端工程师"}); err != nil {
		t.Fatalf("unwired UpsertDomainProfile = %v, want nil", err)
	}

	// GetDomainProfile / ListDomainProfiles：nil 结果无错误。
	if rec, err := s2.GetDomainProfile(ctx, "前端工程师"); err != nil || rec != nil {
		t.Fatalf("GetDomainProfile = (%v, %v), want (nil, nil)", rec, err)
	}
	if recs, err := s2.ListDomainProfiles(ctx); err != nil || recs != nil {
		t.Fatalf("ListDomainProfiles = (%v, %v), want (nil, nil)", recs, err)
	}
	if recs, err := s2.QueryBlockMemoryByDomain(ctx, "前端工程师", 5); err != nil || recs != nil {
		t.Fatalf("QueryBlockMemoryByDomain = (%v, %v), want (nil, nil)", recs, err)
	}

	// DeleteBySource：nil-DB 零删除；空 source 报错（有 db 时才到达，nil 直接短路）。
	n, err := s2.DeleteBySource(ctx, "external:x.md", enums.KnowledgeTypeExternalKB)
	if err != nil || n != 0 {
		t.Fatalf("DeleteBySource = (%d, %v), want (0, nil)", n, err)
	}
}
