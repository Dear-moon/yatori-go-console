package zhihuishu

import (
	"encoding/json"
	"testing"

	"github.com/yatori-dev/yatori-go-core/api/zhihuishu"
)

func TestMasteryAnswerEncoding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    int
		options string
		answers []string
		want    string
		fail    bool
	}{
		{"content", 1, `[{"id":11,"content":"<p>毛泽东</p>"},{"id":22,"content":"七大"}]`, []string{"毛 泽东"}, "11", false},
		{"multiple", 2, `[{"id":11,"content":"甲"},{"id":22,"content":"乙"},{"id":33,"content":"丙"}]`, []string{"C", "A"}, "11#@#33", false},
		{"judge", 14, `[{"id":11,"content":"对"},{"id":22,"content":"错"}]`, []string{"错误"}, "22", false},
		{"missing", 1, `[{"id":11,"content":"甲"}]`, nil, "", true},
		{"unknown", 1, `[{"id":11,"content":"甲"}]`, []string{"乙"}, "", true},
		{"duplicate", 2, `[{"id":11,"content":"甲"}]`, []string{"甲", "A"}, "", true},
		{"single", 1, `[{"id":11,"content":"甲"},{"id":22,"content":"乙"}]`, []string{"甲", "乙"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var q zhihuishu.MasteryQuestion
			if err := json.Unmarshal([]byte(`{"optionVos":`+tc.options+`}`), &q); err != nil {
				t.Fatal(err)
			}
			q.Type = tc.kind
			got, err := encodeMasteryAnswer(q, tc.answers)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("answer = %q, %v", got, err)
			}
		})
	}
}

func TestMasteryText(t *testing.T) {
	text, err := masteryText(`<p>马克思主义&nbsp;中国化</p><p>时代化</p>`)
	if err != nil || text != "马克思主义 中国化 时代化" {
		t.Fatalf("text = %q, %v", text, err)
	}
	if _, err := masteryText(`<p>题干<img src="question.png"></p>`); err == nil {
		t.Fatal("image question was silently reduced to text")
	}
}
