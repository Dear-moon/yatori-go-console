package zhihuishu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLoginChoiceModes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		qr    bool
		quit  bool
	}{
		{"enter", "\n", true, false},
		{"number-qr", "1\n", true, false},
		{"text-qr", "QR\n", true, false},
		{"number-browser", "2\n", false, false},
		{"text-browser", "browser\n", false, false},
		{"cancel", "q\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			qr, err := chooseLoginMethod(context.Background(), bufio.NewReader(strings.NewReader(tc.input)), &output)
			if qr != tc.qr || errors.Is(err, context.Canceled) != tc.quit || (!tc.quit && err != nil) {
				t.Fatalf("choice = %v, %v", qr, err)
			}
			if !strings.Contains(output.String(), "回车或 1 扫码") {
				t.Fatal("normal startup does not expose QR login")
			}
		})
	}
}

func TestLoginChoiceRetriesInvalidInput(t *testing.T) {
	var output bytes.Buffer
	qr, err := chooseLoginMethod(context.Background(), bufio.NewReader(strings.NewReader("invalid\n1\n")), &output)
	if err != nil || !qr || !strings.Contains(output.String(), "无效选择") {
		t.Fatalf("choice = %v, %v", qr, err)
	}
}
