package zhihuishu

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"yatori-go-console/config"

	"github.com/yatori-dev/yatori-go-core/api/zhihuishu"
)

func Run(ctx context.Context, users []config.User, input io.Reader, output io.Writer, proxy func() string) error {
	reader := bufio.NewReaderSize(input, 128)
	for _, user := range users {
		if user.AccountType != "ZHIHUISHU" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		options := zhihuishu.ClientOptions{}
		if user.IsProxy == 1 {
			if proxy == nil {
				return errors.New("智慧树代理不可用")
			}
			options.ProxyURL = proxy()
		}
		client, err := zhihuishu.NewClient(options)
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

func runAccount(ctx context.Context, user config.User, client *zhihuishu.Client, reader *bufio.Reader, output io.Writer) error {
	if user.CoursesCustom.VideoModel != 0 && user.CoursesCustom.VideoModel != 1 {
		return errors.New("智慧树 videoModel 仅支持 0（查看目录）或 1（普通计时模式）")
	}
	if user.CoursesCustom.AutoExam != 0 {
		fmt.Fprintln(output, "[智慧树] 自动答题尚未接入。")
	}
	fmt.Fprintln(output, "[智慧树] 请在专用浏览器登录 onlineweb.zhihuishu.com，并打开 AI 课程章节页面。")
	fmt.Fprintln(output, "默认连接本机 9223 端口；可用 YATORI_ZHIHUISHU_CDP_URL 指定本机调试地址。")
	fmt.Fprint(output, "确认浏览器当前账号无误后按回车继续（输入 q 取消）：")
	type answer struct {
		value string
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		line, err := reader.ReadString('\n')
		done <- answer{strings.TrimSpace(line), err}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-done:
		if result.err != nil {
			return errors.New("无法读取登录确认")
		}
		if result.value == "q" {
			return context.Canceled
		}
	}
	material, err := browserSession(ctx)
	if err != nil {
		return err
	}
	current, err := client.LoginBrowserSession(ctx, *material)
	material = nil
	if err != nil {
		if errors.Is(err, zhihuishu.ErrAuthenticationFailed) || errors.Is(err, zhihuishu.ErrSessionExpired) {
			return fmt.Errorf("请先在浏览器访问 onlineweb.zhihuishu.com 恢复登录，再重新运行：%w", err)
		}
		return err
	}
	if current == nil || current.ID == "" {
		return zhihuishu.ErrAuthenticationFailed
	}
	fmt.Fprintln(output, "[智慧树] 已验证浏览器登录身份。")
	courses, err := client.AICourses(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "[智慧树] 找到 %d 门 AI 课程。\n", len(courses))
	for _, course := range courses {
		settings := user.CoursesCustom
		if config.CmpCourse(course.Name, settings.ExcludeCourses) || (len(settings.IncludeCourses) > 0 && !config.CmpCourse(course.Name, settings.IncludeCourses)) {
			continue
		}
		modules, err := client.Knowledge(ctx, course)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "[智慧树] %s\n", safeText(course.Name))
		count := 0
		for _, module := range modules {
			fmt.Fprintf(output, "  %s\n", safeText(module.Name))
			for _, unit := range module.Units {
				fmt.Fprintf(output, "    %s（%d 个知识点）\n", safeText(unit.Name), len(unit.Points))
				count += len(unit.Points)
				if settings.VideoModel == 1 {
					for _, point := range unit.Points {
						resources, err := client.Resources(ctx, course, point.ID)
						if err != nil {
							return err
						}
						for _, resource := range resources {
							if resource.Status == 1 {
								continue
							}
							fmt.Fprintf(output, "[智慧树] %s：%s\n", safeText(point.Name), safeText(resource.Name))
							switch resource.DataType {
							case 11:
								err = client.StudyVideo(ctx, course, point.ID, resource, func(position, total int) {
									fmt.Fprintf(output, "[智慧树] 视频断点／服务端已接受进度：%d/%d 秒\n", position, total)
								})
							case 21:
								err = client.CompleteBook(ctx, course, point.ID, resource)
								if err == nil {
									fmt.Fprintln(output, "[智慧树] 服务端已接受电子书预览回执。")
								}
							default:
								err = zhihuishu.ErrUnsupportedResource
							}
							if errors.Is(err, zhihuishu.ErrUnsupportedResource) {
								fmt.Fprintln(output, "[智慧树] 此资源类型或片段格式尚未支持，请在浏览器处理。")
								continue
							}
							if err != nil {
								return err
							}
						}
					}
				}
			}
		}
		fmt.Fprintf(output, "[智慧树] 已读取 %d 个知识点。\n", count)
	}
	return nil
}

func safeText(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
}
