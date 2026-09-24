package mooc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	moocstudy "github.com/yatori-dev/yatori-go-core/aggregation/mooc"
	"github.com/yatori-dev/yatori-go-core/api/mooc"
	"yatori-go-console/config"
)

type session interface {
	moocstudy.VideoClient
	CompleteDocument(context.Context, mooc.MOOCCourse, mooc.MOOCLesson, mooc.MOOCUnit) error
	LoginCookies(context.Context, []*http.Cookie) (*mooc.MOOCUser, error)
	Courses(context.Context) ([]mooc.MOOCCourse, error)
	Chapters(context.Context, string) ([]mooc.MOOCChapter, error)
	Close()
}
type sessionFactory func(string, mooc.ClientOptions) (session, error)

func Run(ctx context.Context, users []config.User, input io.Reader, output io.Writer, proxy func() string) error {
	return run(ctx, users, input, output, proxy, func(account string, options mooc.ClientOptions) (session, error) {
		return mooc.NewCookieClient(options)
	})
}

func run(ctx context.Context, users []config.User, input io.Reader, output io.Writer, proxy func() string, newSession sessionFactory) error {
	reader := bufio.NewReaderSize(input, 128)
	for _, user := range users {
		if user.AccountType != "MOOC" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		options := mooc.ClientOptions{}
		if user.IsProxy == 1 {
			if proxy == nil {
				return errors.New("MOOC proxy is unavailable")
			}
			options.IpProxySW = true
			options.ProxyIP = proxy()
		}
		client, err := newSession(user.Account, options)
		if err != nil {
			return err
		}
		err = runAccount(ctx, user, client, reader, output)
		client.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func runAccount(ctx context.Context, user config.User, client session, reader *bufio.Reader, output io.Writer) error {
	if user.CoursesCustom.VideoModel != 0 && user.CoursesCustom.VideoModel != 1 {
		return errors.New("MOOC videoModel supports only 0 (disabled) or 1 (elapsed-time reporting)")
	}
	if user.CoursesCustom.AutoExam != 0 {
		fmt.Fprintln(output, "[MOOC] 自动答题尚未接入，本次不会提交测试。")
	}
	label := user.RemarkName
	if label == "" {
		label = maskAccount(user.Account)
	}
	fmt.Fprintf(output, "[MOOC] %s：请在启用远程调试的专用浏览器中登录对应账号。\n", safeText(label))
	fmt.Fprintln(output, "默认连接本机 9222 端口；可用 YATORI_MOOC_CDP_URL 指定本机调试地址。")
	fmt.Fprint(output, "确认浏览器当前账号无误后按回车继续（输入 q 取消）：")
	confirmation, err := readCode(ctx, reader)
	if err != nil {
		return err
	}
	if confirmation == "q" {
		return context.Canceled
	}
	cookies, err := browserCookies(ctx)
	if err != nil {
		return err
	}
	current, err := client.LoginCookies(ctx, cookies)
	cookies = nil
	if err != nil {
		return explainError(err)
	}
	if current == nil || current.ID == "" {
		return mooc.ErrAuthenticationUnverified
	}
	fmt.Fprintf(output, "[MOOC] 已验证身份：%s\n", safeText(current.Nickname))
	fmt.Fprintln(output, "[MOOC] 登录态已导入，可关闭专用浏览器；后续任务由 CLI 自动执行。")
	courses, err := client.Courses(ctx)
	if err != nil {
		return explainError(err)
	}
	if len(courses) == 0 {
		fmt.Fprintln(output, "[MOOC] 暂无已选 MOOC 课程。")
		return nil
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "序号\t课程\t学校\t课程 ID\t学期 ID")
	for i, course := range courses {
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\n", i+1, safeText(course.Name), safeText(course.School), safeText(course.ID), safeText(course.TermID))
	}
	if err := table.Flush(); err != nil {
		return err
	}
	for _, course := range courses {
		settings := user.CoursesCustom
		if config.CmpCourse(course.Name, settings.ExcludeCourses) || (len(settings.IncludeCourses) > 0 && !config.CmpCourse(course.Name, settings.IncludeCourses)) {
			continue
		}
		chapters, err := client.Chapters(ctx, course.TermID)
		if err != nil {
			return explainError(err)
		}
		fmt.Fprintf(output, "[MOOC] %s\n", safeText(course.Name))
		count := 0
		for _, chapter := range chapters {
			fmt.Fprintf(output, "  %s\n", safeText(chapter.Name))
			for _, lesson := range chapter.Lessons {
				fmt.Fprintf(output, "    %s (%d 个学习单元)\n", safeText(lesson.Name), len(lesson.Units))
				count += len(lesson.Units)
				if settings.VideoModel == 1 {
					for _, unit := range lesson.Units {
						if unit.ContentType == 3 {
							if unit.ViewStatus == nil || *unit.ViewStatus != 5 {
								timer := time.NewTimer(2 * time.Second)
								select {
								case <-ctx.Done():
									timer.Stop()
									return ctx.Err()
								case <-timer.C:
								}
							}
							if err := client.CompleteDocument(ctx, course, lesson, unit); err != nil {
								return explainError(err)
							}
							fmt.Fprintf(output, "[MOOC] %s：已确认服务端文档已学习标记。\n", safeText(unit.Name))
							continue
						}
						if unit.ContentType != 1 {
							fmt.Fprintf(output, "[MOOC] %s：跳过未支持的学习类型 %d\n", safeText(unit.Name), unit.ContentType)
							continue
						}
						fmt.Fprintf(output, "[MOOC] %s：开始普通模式计时上报\n", safeText(unit.Name))
						options := moocstudy.StudyOptions{OnProgress: func(position, total int) {
							fmt.Fprintf(output, "[MOOC] %s：服务端已接受 %d/%d 秒进度\n", safeText(unit.Name), position, total)
						}}
						if err := moocstudy.StudyVideo(ctx, client, course, lesson, unit, options); err != nil {
							return explainError(err)
						}
					}
				}
			}
		}
		fmt.Fprintf(output, "[MOOC] 已处理 %d 个学习单元的目录；视频与文档按 videoModel 执行，其他类型任务尚未自动处理。\n", count)
	}
	return nil
}
func readCode(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) > 0 {
			err = nil
		}
		done <- result{strings.TrimSpace(string(line)), err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		if r.err != nil {
			return "", errors.New("无法读取登录确认")
		}
		return r.line, nil
	}
}
func maskAccount(account string) string {
	if len(account) < 7 {
		return "已配置账号"
	}
	return account[:3] + "****" + account[len(account)-4:]
}
func safeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
}
func explainError(err error) error {
	if errors.Is(err, mooc.ErrNeedsUserAction) {
		return fmt.Errorf("平台要求额外人工验证，请在浏览器完成；完成后请重新运行以导入 Cookie：%w", err)
	}
	return err
}
