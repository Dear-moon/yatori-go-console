package zhihuishu

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"yatori-go-console/config"
	"yatori-go-console/logic/platform"

	"github.com/yatori-dev/yatori-go-core/api/zhihuishu"
)

func Prepare(ctx context.Context, user config.User, reader *bufio.Reader, output io.Writer, proxy func() string) (*platform.Account, error) {
	options := zhihuishu.ClientOptions{}
	if user.IsProxy == 1 {
		options.ProxyURL = proxy()
	}
	client, err := zhihuishu.NewClient(options)
	if err != nil {
		return nil, err
	}
	account, err := prepareAccount(ctx, user, client, reader, output)
	if err != nil {
		client.Close()
		return nil, err
	}
	account.Close = client.Close
	return account, nil
}

func prepareAccount(ctx context.Context, user config.User, client *zhihuishu.Client, reader *bufio.Reader, output io.Writer) (*platform.Account, error) {
	if user.CoursesCustom.VideoModel != 0 && user.CoursesCustom.VideoModel != 1 {
		return nil, errors.New("智慧树 videoModel 仅支持 0（查看目录）或 1（普通计时模式）")
	}
	if user.CoursesCustom.AutoExam != 0 {
		fmt.Fprintln(output, "[智慧树] 自动答题尚未接入。")
	}
	useQR := strings.EqualFold(strings.TrimSpace(os.Getenv("YATORI_ZHIHUISHU_LOGIN")), "qr")
	if !useQR {
		selectedQR, err := chooseLoginMethod(ctx, reader, output)
		if err != nil {
			return nil, err
		}
		useQR = selectedQR
	}
	current, err := loginZhihuishu(ctx, client, output, useQR)
	if err != nil {
		if errors.Is(err, zhihuishu.ErrAuthenticationFailed) || errors.Is(err, zhihuishu.ErrSessionExpired) {
			return nil, fmt.Errorf("智慧树登录态无效，请在浏览器恢复登录，或用 YATORI_ZHIHUISHU_LOGIN=qr 扫码登录：%w", err)
		}
		return nil, err
	}
	if current == nil || current.ID == "" {
		return nil, zhihuishu.ErrAuthenticationFailed
	}

	courses, err := client.AICourses(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(output, "[智慧树] 找到 %d 门 AI 课程。\n", len(courses))
	account := &platform.Account{}
	for _, course := range courses {
		account.Courses = append(account.Courses, platform.Course{Name: course.Name, Detail: "AI 课程"})
	}
	account.Study = func(ctx context.Context) error {
		return studyAccount(ctx, user, client, courses, output)
	}
	return account, nil
}

func chooseLoginMethod(ctx context.Context, reader *bufio.Reader, output io.Writer) (bool, error) {
	fmt.Fprintln(output, "[智慧树] 登录方式：1 扫码登录（推荐），2 导入专用浏览器登录态，q 取消。")
	fmt.Fprintln(output, "[智慧树] 浏览器方式需先在专用浏览器登录 onlineweb.zhihuishu.com，默认调试端口 9223。")
	for {
		fmt.Fprint(output, "请选择登录方式（回车或 1 扫码，2 浏览器，q 取消）：")
		type answer struct {
			value string
			err   error
		}
		done := make(chan answer, 1)
		go func() {
			line, err := reader.ReadString('\n')
			done <- answer{strings.ToLower(strings.TrimSpace(line)), err}
		}()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case result := <-done:
			if result.err != nil {
				return false, errors.New("无法读取登录方式")
			}
			switch result.value {
			case "", "1", "qr":
				return true, nil
			case "2", "browser":
				return false, nil
			case "q":
				return false, context.Canceled
			default:
				fmt.Fprintln(output, "[智慧树] 无效选择，请输入 1、2 或 q。")
			}
		}
	}
}

func studyAccount(ctx context.Context, user config.User, client *zhihuishu.Client, courses []zhihuishu.AICourse, output io.Writer) error {
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
					if len(unit.Points) == 0 {
						return fmt.Errorf("章节“%s”没有可核验的知识点，已停止自动切换", safeText(unit.Name))
					}
					fmt.Fprintf(output, "[智慧树] 进入章节：%s / %s\n", safeText(module.Name), safeText(unit.Name))
					for _, point := range unit.Points {
						if err := ctx.Err(); err != nil {
							return err
						}
						fmt.Fprintf(output, "[智慧树] 进入知识点：%s\n", safeText(point.Name))
						resources, err := client.Resources(ctx, course, point.ID)
						if err != nil {
							return err
						}
						for _, resource := range resources {
							if resource.Status == 1 {
								continue
							}
							fmt.Fprintf(output, "[智慧树] %s：%s\n", safeText(point.Name), safeText(resource.Name))
							switch resource.Kind() {
							case zhihuishu.ResourceVideo:
								err = client.StudyVideo(ctx, course, point.ID, resource, func(position, total int) {
									fmt.Fprintf(output, "[智慧树] 视频断点／服务端已接受进度：%d/%d 秒\n", position, total)
								})
							case zhihuishu.ResourcePPT:
								err = client.CompletePPT(ctx, course, point.ID, resource)
								if err == nil {
									fmt.Fprintln(output, "[智慧树] PPT 预览回执已确认完成。")
								}
							case zhihuishu.ResourceBook:
								err = client.CompleteBook(ctx, course, point.ID, resource)
								if err == nil {
									fmt.Fprintln(output, "[智慧树] 服务端已接受电子书预览回执。")
								}
							default:
								err = zhihuishu.ErrUnsupportedResource
							}
							if errors.Is(err, zhihuishu.ErrUnsupportedResource) {
								fmt.Fprintln(output, "[智慧树] 此资源当前不可用，或类型／片段格式尚未支持，请在浏览器核对。")
								continue
							}
							if err != nil {
								return err
							}
						}
						updated, err := client.Resources(ctx, course, point.ID)
						if err != nil {
							return err
						}
						completed := 0
						for _, resource := range updated {
							if resource.Status == 1 {
								completed++
							}
						}
						fmt.Fprintf(output, "[智慧树] %s：已确认完成 %d/%d 项资源。\n", safeText(point.Name), completed, len(updated))
						if !resourcesComplete(resources, updated) {
							return fmt.Errorf("知识点“%s”仍有未完成或无法核验的任务，已停止自动切换；处理后重新运行即可从未完成项继续", safeText(point.Name))
						}
					}
					fmt.Fprintf(output, "[智慧树] 章节“%s”全部任务已确认完成，继续下一章节。\n", safeText(unit.Name))
				}
			}
		}
		fmt.Fprintf(output, "[智慧树] 已读取 %d 个知识点。\n", count)
		if settings.VideoModel == 1 && count > 0 {
			fmt.Fprintln(output, "[智慧树] 本课程目录内的资源任务已全部确认完成。")
		}
	}
	return nil
}

func resourcesComplete(before, after []zhihuishu.Resource) bool {
	if len(before) == 0 || len(after) == 0 {
		return false
	}
	confirmed := make(map[string]bool, len(after))
	for _, resource := range after {
		if resource.ID == "" || resource.Status != 1 || confirmed[resource.ID] {
			return false
		}
		confirmed[resource.ID] = true
	}
	// A disappearing task must not make an incomplete knowledge point look complete.
	for _, resource := range before {
		if !confirmed[resource.ID] {
			return false
		}
	}
	return true
}

// loginZhihuishu prefers browser cookies and falls back to a scanned code.
func loginZhihuishu(ctx context.Context, client *zhihuishu.Client, output io.Writer, useQR bool) (*zhihuishu.User, error) {
	if !useQR {
		cookies, err := browserCookies(ctx)
		if err == nil {
			user, err := client.LoginCookies(ctx, cookies)
			cookies = nil
			if err == nil {
				fmt.Fprintln(output, "[智慧树] 登录态已导入并验证，可关闭专用浏览器；后续任务由 CLI 自动执行。")
				return user, nil
			}
			fmt.Fprintln(output, "[智慧树] 浏览器登录态不可用，改用扫码登录：", safeText(err.Error()))
		} else {
			fmt.Fprintln(output, "[智慧树] 未找到可用的调试浏览器，改用扫码登录。")
		}
	}
	fmt.Fprintln(output, "[智慧树] 正在获取登录二维码…")
	user, err := client.LoginQRCode(ctx, func(code zhihuishu.QRCode) error { return showQRCode(output, code) }, 0)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(output, "[智慧树] 扫码登录成功，后续任务由 CLI 自动执行。")
	return user, nil
}

// showQRCode stores the image and opens it with the default viewer.
func showQRCode(output io.Writer, code zhihuishu.QRCode) error {
	path := filepath.Join("assets", "qr", "zhihuishu-login.png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, code.ImagePNG, 0o600); err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	fmt.Fprintf(output, "[智慧树] 请用智慧树/知到 App 扫码登录，二维码文件：%s（有效期约 3 分钟）\n", absolute)
	if runtime.GOOS == "windows" {
		_ = exec.Command("cmd", "/c", "start", "", absolute).Start()
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
