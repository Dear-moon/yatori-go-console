package console

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"yatori-go-console/config"
	"yatori-go-console/global"
	"yatori-go-console/logic/mooc"
	"yatori-go-console/logic/platform"
	"yatori-go-console/logic/zhihuishu"
)

type accountEntry struct {
	user    config.User
	account *platform.Account
	err     error
}

type platformGroup struct {
	kind     string
	accounts []*accountEntry
}

type accountResult struct {
	entry   *accountEntry
	account *platform.Account
	err     error
}

type prepareFunc func(context.Context, config.User, *bufio.Reader, io.Writer) (*platform.Account, error)

func Run(ctx context.Context, data *config.JSONDataForConfig, input io.Reader, output io.Writer, proxy func() string) error {
	return run(ctx, data, input, output, func(ctx context.Context, user config.User, reader *bufio.Reader, out io.Writer) (*platform.Account, error) {
		switch user.AccountType {
		case "MOOC":
			return mooc.Prepare(ctx, user, reader, out, proxy)
		case "ZHIHUISHU":
			return zhihuishu.Prepare(ctx, user, reader, out, proxy)
		default:
			return prepareLegacy(ctx, data.Setting, user, proxy)
		}
	})
}

func run(ctx context.Context, data *config.JSONDataForConfig, input io.Reader, output io.Writer, prepare prepareFunc) error {
	out := &lockedWriter{output: output}
	reader := bufio.NewReader(input)
	groups := groupAccounts(data.Users)
	defer func() {
		for _, group := range groups {
			for _, entry := range group.accounts {
				if entry.account != nil && entry.account.Close != nil {
					entry.account.Close()
				}
			}
		}
	}()

	fmt.Fprintln(out, "正在并发登录并查询已登记平台的课程；选择平台前不会提交学习进度。")
	results := make(chan accountResult, len(data.Users))
	var inputLock sync.Mutex
	for _, group := range groups {
		go func() {
			for _, entry := range group.accounts {
				accountOut := &taggedWriter{output: out, label: accountLabel(entry.user), lineStart: true}
				interactive := entry.user.AccountType == "ZHIHUISHU" || (entry.user.AccountType == "MOOC" && (strings.TrimSpace(entry.user.Account) == "" || strings.TrimSpace(entry.user.Password) == ""))
				if interactive {
					inputLock.Lock()
				}
				account, err := prepare(ctx, entry.user, reader, accountOut)
				if interactive {
					inputLock.Unlock()
				}
				results <- accountResult{entry: entry, account: account, err: err}
			}
		}()
	}
	for range data.Users {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			result.entry.account, result.entry.err = result.account, result.err
			if result.err != nil {
				fmt.Fprintf(out, "[查询失败] %s：%v\n", accountLabel(result.entry.user), result.err)
			} else {
				fmt.Fprintf(out, "[查询完成] %s：%d 门课程\n", accountLabel(result.entry.user), len(result.account.Courses))
			}
		}
	}

	for {
		showMenu(out, groups)
		line, err := readLine(ctx, reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(line), "q") {
			return nil
		}
		selected, err := parseSelection(line, len(groups))
		if err != nil {
			fmt.Fprintln(out, err)
			continue
		}
		if err := runBatch(ctx, groups, selected, out); err != nil {
			return err
		}
	}
}

func groupAccounts(users []config.User) []platformGroup {
	var groups []platformGroup
	indices := make(map[string]int)
	for _, user := range users {
		index, ok := indices[user.AccountType]
		if !ok {
			index = len(groups)
			indices[user.AccountType] = index
			groups = append(groups, platformGroup{kind: user.AccountType})
		}
		groups[index].accounts = append(groups[index].accounts, &accountEntry{user: user})
	}
	return groups
}

func showMenu(out io.Writer, groups []platformGroup) {
	fmt.Fprintln(out, "\n平台课程一览（启动时查询结果）")
	for i, group := range groups {
		fmt.Fprintf(out, "%d. %s\n", i+1, platformName(group.kind))
		for _, entry := range group.accounts {
			if entry.err != nil {
				fmt.Fprintf(out, "   %s：不可用（%v）\n", accountLabel(entry.user), entry.err)
				continue
			}
			fmt.Fprintf(out, "   %s：%d 门课程，videoModel=%d\n", accountLabel(entry.user), len(entry.account.Courses), entry.user.CoursesCustom.VideoModel)
			for _, course := range entry.account.Courses {
				fmt.Fprintf(out, "     - %s", safeText(course.Name))
				if course.Detail != "" {
					fmt.Fprintf(out, "（%s）", safeText(course.Detail))
				}
				fmt.Fprintln(out)
			}
		}
	}
	fmt.Fprintln(out, "任务沿用配置中的课程筛选和学习模式；同平台账号依次执行，不同平台同时执行。")
	fmt.Fprint(out, "选择平台编号（如 1,3；all 全部；q 退出）：")
}

func parseSelection(line string, count int) ([]int, error) {
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "all") {
		selected := make([]int, count)
		for i := range selected {
			selected[i] = i
		}
		return selected, nil
	}
	parts := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\t' })
	if len(parts) == 0 {
		return nil, errors.New("请输入平台编号、all 或 q；空输入不会启动任务。")
	}
	var selected []int
	seen := make(map[int]bool)
	for _, part := range parts {
		index, err := strconv.Atoi(part)
		if err != nil || index < 1 || index > count {
			return nil, fmt.Errorf("无效平台编号 %q，请输入 1 到 %d。", part, count)
		}
		if !seen[index] {
			seen[index] = true
			selected = append(selected, index-1)
		}
	}
	return selected, nil
}

func runBatch(ctx context.Context, groups []platformGroup, selected []int, out io.Writer) error {
	results := make(chan accountResult)
	var tasks [][]*accountEntry
	total := 0
	for _, index := range selected {
		var ready []*accountEntry
		for _, entry := range groups[index].accounts {
			if entry.err == nil && len(entry.account.Courses) > 0 {
				ready = append(ready, entry)
			}
		}
		if len(ready) == 0 {
			fmt.Fprintf(out, "[跳过] %s：没有查询成功且有课程的账号。\n", platformName(groups[index].kind))
			continue
		}
		tasks = append(tasks, ready)
		total += len(ready)
		fmt.Fprintf(out, "[平台启动] %s：%d 个账号\n", platformName(groups[index].kind), len(ready))
	}
	if total == 0 {
		fmt.Fprintln(out, "没有可启动的任务。")
		return nil
	}
	for _, entries := range tasks {
		go func() {
			for _, entry := range entries {
				err := entry.account.Study(ctx)
				select {
				case results <- accountResult{entry: entry, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	for range total {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-results:
			if result.err != nil {
				fmt.Fprintf(out, "[任务失败] %s：%v\n", accountLabel(result.entry.user), result.err)
			} else {
				fmt.Fprintf(out, "[任务返回] %s：本轮任务入口已返回，请以各平台进度日志为准。\n", accountLabel(result.entry.user))
			}
		}
	}
	fmt.Fprintln(out, "本轮所选平台已全部结束，返回选择菜单。")
	return nil
}

func readLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	type answer struct {
		line string
		err  error
	}
	result := make(chan answer, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) && len(line) > 0 {
			err = nil
		}
		result <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case answer := <-result:
		return answer.line, answer.err
	}
}

func platformName(kind string) string {
	if name, ok := global.AccountTypeStr[kind]; ok {
		return name
	}
	return safeText(kind)
}

func accountLabel(user config.User) string {
	label := user.RemarkName
	if label == "" {
		label = user.Account
		if len(label) >= 7 {
			label = label[:3] + "****" + label[len(label)-4:]
		}
	}
	return platformName(user.AccountType) + " / " + safeText(label)
}

func safeText(text string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, text)
}

type lockedWriter struct {
	mu     sync.Mutex
	output io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.output.Write(p)
}

type taggedWriter struct {
	output    io.Writer
	label     string
	lineStart bool
}

func (w *taggedWriter) Write(p []byte) (int, error) {
	var text strings.Builder
	for _, part := range strings.SplitAfter(string(p), "\n") {
		if part == "" {
			continue
		}
		if w.lineStart {
			fmt.Fprintf(&text, "[%s] ", w.label)
		}
		text.WriteString(part)
		w.lineStart = strings.HasSuffix(part, "\n")
	}
	if _, err := io.WriteString(w.output, text.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
