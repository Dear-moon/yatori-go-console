package console

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"yatori-go-console/config"
	"yatori-go-console/logic/platform"
)

func TestParseSelection(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []int
	}{
		{"1,3", []int{0, 2}},
		{"2，1 2", []int{1, 0}},
		{"all", []int{0, 1, 2}},
	} {
		got, err := parseSelection(tc.input, 3)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("parseSelection(%q) = %v, %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"", "0", "4", "1,no", "-1"} {
		if _, err := parseSelection(input, 3); err == nil {
			t.Fatalf("accepted invalid selection %q", input)
		}
	}
}

type menuWriter struct {
	bytes.Buffer
	menus chan struct{}
}

func (w *menuWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if strings.Contains(string(p), "选择平台编号") {
		w.menus <- struct{}{}
	}
	return n, err
}

func await[T any](t *testing.T, ctx context.Context, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for console event")
		var zero T
		return zero
	}
}

func TestQueryBeforeSelectionAndSessionReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data := &config.JSONDataForConfig{Users: []config.User{
		{AccountType: "XUEXITONG"},
		{AccountType: "MOOC", Account: "fixture", Password: "fixture"},
		{AccountType: "ZHIHUISHU"},
	}}
	started := make(chan string, 3)
	release := make(chan struct{})
	var prepared, studied, closed atomic.Int32
	prepare := func(ctx context.Context, user config.User, _ *bufio.Reader, _ io.Writer) (*platform.Account, error) {
		prepared.Add(1)
		started <- user.AccountType
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &platform.Account{
			Courses: []platform.Course{{Name: "fixture"}},
			Study:   func(context.Context) error { studied.Add(1); return nil },
			Close:   func() { closed.Add(1) },
		}, nil
	}
	input, control := io.Pipe()
	defer input.Close()
	defer control.Close()
	out := &menuWriter{menus: make(chan struct{}, 4)}
	done := make(chan error, 1)
	go func() { done <- run(ctx, data, input, out, prepare) }()
	for range 3 {
		await(t, ctx, started)
	}
	close(release)
	await(t, ctx, out.menus)
	if studied.Load() != 0 {
		t.Fatal("querying courses started study")
	}
	if _, err := io.WriteString(control, "1,2,3\n"); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, out.menus)
	if studied.Load() != 3 {
		t.Fatalf("studied %d accounts", studied.Load())
	}
	if _, err := io.WriteString(control, "1\n"); err != nil {
		t.Fatal(err)
	}
	await(t, ctx, out.menus)
	if studied.Load() != 4 || prepared.Load() != 3 {
		t.Fatal("returning to the menu did not reuse prepared sessions")
	}
	if _, err := io.WriteString(control, "q\n"); err != nil {
		t.Fatal(err)
	}
	if err := await(t, ctx, done); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 3 {
		t.Fatalf("closed %d sessions", closed.Load())
	}
}

func TestBatchRunsPlatformsConcurrentlyAndAccountsSequentially(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 3)
	release := make(chan struct{})
	entry := func(name string, wait bool) *accountEntry {
		return &accountEntry{account: &platform.Account{
			Courses: []platform.Course{{Name: name}},
			Study: func(ctx context.Context) error {
				started <- name
				if wait {
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			},
		}}
	}
	groups := []platformGroup{
		{kind: "XUEXITONG", accounts: []*accountEntry{entry("a1", true), entry("a2", false)}},
		{kind: "MOOC", accounts: []*accountEntry{entry("b", true)}},
	}
	done := make(chan error, 1)
	go func() { done <- runBatch(ctx, groups, []int{0, 1}, io.Discard) }()
	first, second := await(t, ctx, started), await(t, ctx, started)
	if !((first == "a1" && second == "b") || (first == "b" && second == "a1")) {
		t.Fatalf("unexpected starts: %s, %s", first, second)
	}
	close(release)
	if name := await(t, ctx, started); name != "a2" {
		t.Fatalf("last start = %s", name)
	}
	if err := await(t, ctx, done); err != nil {
		t.Fatal(err)
	}
}

func TestFailedQueryDoesNotPreventOtherPlatforms(t *testing.T) {
	var studied atomic.Int32
	data := &config.JSONDataForConfig{Users: []config.User{{AccountType: "CQIE"}, {AccountType: "WELEARN"}}}
	prepare := func(_ context.Context, user config.User, _ *bufio.Reader, _ io.Writer) (*platform.Account, error) {
		if user.AccountType == "CQIE" {
			return nil, errors.New("fixture query failure")
		}
		return &platform.Account{Courses: []platform.Course{{Name: "fixture"}}, Study: func(context.Context) error { studied.Add(1); return nil }}, nil
	}
	var output bytes.Buffer
	if err := run(context.Background(), data, strings.NewReader("all\nq\n"), &output, prepare); err != nil {
		t.Fatal(err)
	}
	if studied.Load() != 1 || !strings.Contains(output.String(), "[查询失败]") || !strings.Contains(output.String(), "[跳过]") {
		t.Fatal("failed platform interfered with the available platform")
	}
}

func TestInvalidInputAndQuitDoNotStartStudy(t *testing.T) {
	var studied int
	prepare := func(context.Context, config.User, *bufio.Reader, io.Writer) (*platform.Account, error) {
		return &platform.Account{Courses: []platform.Course{{Name: "fixture"}}, Study: func(context.Context) error { studied++; return nil }}, nil
	}
	data := &config.JSONDataForConfig{Users: []config.User{{AccountType: "XUEXITONG"}}}
	if err := run(context.Background(), data, strings.NewReader("\n99\nq\n"), io.Discard, prepare); err != nil {
		t.Fatal(err)
	}
	if studied != 0 {
		t.Fatal("invalid input started study")
	}
}

func TestLegacyQueryDoesNotStudy(t *testing.T) {
	var studied int
	account, err := loadLegacy(context.Background(), func() error { return nil }, func() ([]string, error) { return []string{"fixture"}, nil },
		func(name string) platform.Course { return platform.Course{Name: name} }, func() { studied++ })
	if err != nil || studied != 0 || len(account.Courses) != 1 {
		t.Fatalf("legacy preparation = %v, studied=%d", err, studied)
	}
	if err := account.Study(context.Background()); err != nil || studied != 1 {
		t.Fatalf("legacy study = %v, studied=%d", err, studied)
	}
}
