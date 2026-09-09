package domain_test

import (
	"strings"
	"testing"

	"goseek/internal/domain"
)

func TestNewSessionIDIsValidAndUnique(t *testing.T) {
	seen := make(map[domain.SessionID]struct{})
	for range 100 {
		id, err := domain.NewSessionID()
		if err != nil {
			t.Fatalf("NewSessionID 返回错误: %v", err)
		}
		if err := id.Validate(); err != nil {
			t.Fatalf("新生成的 ID %q 没有通过校验: %v", id, err)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("生成了重复的 ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

// ID 会被拼进文件路径，所以校验必须挡住任何不是纯十六进制的取值。
func TestValidateRejectsUnusableIDs(t *testing.T) {
	cases := []struct {
		name string
		id   domain.SessionID
	}{
		{"空", ""},
		{"没有前缀", "0123456789abcdef0123456789abcdef"},
		{"错误前缀", "sess_0123456789abcdef0123456789abcdef"},
		{"太短", "ses_0123456789abcdef"},
		{"太长", "ses_0123456789abcdef0123456789abcdef00"},
		{"大写十六进制", "ses_0123456789ABCDEF0123456789abcdef"},
		{"含非十六进制字符", "ses_0123456789abcdef0123456789abcdeg"},
		{"路径穿越", "ses_../../../etc/passwd"},
		{"含斜杠", "ses_0123456789abcdef0123456789abc/def"},
		{"只有前缀", "ses_"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.id.Validate(); err == nil {
				t.Errorf("ID %q 通过了校验", testCase.id)
			}
		})
	}
}

func TestShortKeepsPrefixAndTruncates(t *testing.T) {
	id := domain.SessionID("ses_1f3c56789abcdef0123456789abcdef0")

	short := id.Short()
	if !strings.HasPrefix(short, "ses_1f3c") {
		t.Errorf("Short() = %q; want 以 ses_1f3c 开头", short)
	}
	if len([]rune(short)) >= len(string(id)) {
		t.Errorf("Short() = %q 没有比完整 ID 更短", short)
	}
}

// 短到不需要截断时原样返回，不产生一个只有省略号的字符串。
func TestShortLeavesAlreadyShortValuesAlone(t *testing.T) {
	if got := domain.SessionID("ses_").Short(); got != "ses_" {
		t.Errorf("Short() = %q; want %q", got, "ses_")
	}
}
