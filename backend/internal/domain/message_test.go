package domain_test

import (
	"testing"

	"goseek/internal/domain"
)

func TestRoleMapsToModelRole(t *testing.T) {
	cases := []struct {
		role domain.Role
		want domain.ModelRole
	}{
		{domain.RoleUser, domain.ModelRoleUser},
		{domain.RoleAssistant, domain.ModelRoleAssistant},
	}
	for _, testCase := range cases {
		if got := testCase.role.ModelRole(); got != testCase.want {
			t.Errorf("Role(%q).ModelRole() = %q; want %q", testCase.role, got, testCase.want)
		}
	}
}

// 会话历史里不存在 system 角色：它只由 ContextManager 生成，且只出现在模型视图中。
// 这个测试锁住两个角色集合的差异，防止后续把 system 误加进 Role。
func TestConversationRolesExcludeSystem(t *testing.T) {
	for _, role := range []domain.Role{domain.RoleUser, domain.RoleAssistant} {
		if role.ModelRole() == domain.ModelRoleSystem {
			t.Fatalf("conversation role %q must not map to the system model role", role)
		}
	}
}
