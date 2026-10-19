package console

import (
	"context"
	"fmt"
	"time"
	"yatori-go-console/config"
	"yatori-go-console/logic/cqie"
	"yatori-go-console/logic/enaea"
	"yatori-go-console/logic/haiqikeji"
	"yatori-go-console/logic/icve"
	"yatori-go-console/logic/ketangx"
	"yatori-go-console/logic/platform"
	qsxt "yatori-go-console/logic/qingshuxuetang"
	"yatori-go-console/logic/welearn"
	"yatori-go-console/logic/xuexitong"
	"yatori-go-console/logic/yinghua"

	cqieAction "github.com/yatori-dev/yatori-go-core/aggregation/cqie"
	enaeaAction "github.com/yatori-dev/yatori-go-core/aggregation/enaea"
	hqkjAction "github.com/yatori-dev/yatori-go-core/aggregation/haiqikeji"
	icveAction "github.com/yatori-dev/yatori-go-core/aggregation/icve"
	ketangxAction "github.com/yatori-dev/yatori-go-core/aggregation/ketangx"
	qsxtAction "github.com/yatori-dev/yatori-go-core/aggregation/qingshuxuetang"
	welearnAction "github.com/yatori-dev/yatori-go-core/aggregation/welearn"
	xxtAction "github.com/yatori-dev/yatori-go-core/aggregation/xuexitong"
	yinghuaAction "github.com/yatori-dev/yatori-go-core/aggregation/yinghua"
	cqieAPI "github.com/yatori-dev/yatori-go-core/api/cqie"
	enaeaAPI "github.com/yatori-dev/yatori-go-core/api/enaea"
	hqkjAPI "github.com/yatori-dev/yatori-go-core/api/haiqikeji"
	icveAPI "github.com/yatori-dev/yatori-go-core/api/icve"
	ketangxAPI "github.com/yatori-dev/yatori-go-core/api/ketangx"
	qsxtAPI "github.com/yatori-dev/yatori-go-core/api/qingshuxuetang"
	welearnAPI "github.com/yatori-dev/yatori-go-core/api/welearn"
	xxtAPI "github.com/yatori-dev/yatori-go-core/api/xuexitong"
	yinghuaAPI "github.com/yatori-dev/yatori-go-core/api/yinghua"
	lg "github.com/yatori-dev/yatori-go-core/utils/log"
)

func prepareLegacy(ctx context.Context, setting config.Setting, user config.User, proxy func() string) (*platform.Account, error) {
	users := []config.User{user}
	switch user.AccountType {
	case "XUEXITONG":
		cache := &xxtAPI.XueXiTUserCache{Name: user.Account, Password: user.Password}
		if user.IsProxy == 1 {
			cache.IpProxySW, cache.ProxyIP = true, "http://"+proxy()
		}
		return loadLegacy(ctx, func() error {
			if len(cache.Password) < 50 {
				return xxtAction.XueXiTLoginAction(cache)
			}
			return xxtAction.XueXiTCookieLoginAction(cache)
		}, func() ([]xxtAction.XueXiTCourse, error) {
			return xxtAction.XueXiTPullCourseActionContext(ctx, cache)
		}, func(c xxtAction.XueXiTCourse) platform.Course {
			detail := fmt.Sprintf("任务点 %d/%d", c.JobFinishCount, c.JobCount)
			if !c.IsStart {
				detail += "，未开课"
			}
			if c.State == 1 {
				detail += "，已结束"
			}
			return platform.Course{Name: c.CourseName, Detail: detail}
		}, func() { xuexitong.RunBrushOperation(setting, users, []*xxtAPI.XueXiTUserCache{cache}) })
	case "YINGHUA":
		cache := &yinghuaAPI.YingHuaUserCache{PreUrl: user.URL, Account: user.Account, Password: user.Password}
		if user.IsProxy == 1 {
			cache.IpProxySW, cache.ProxyIP = true, proxy()
		}
		account, err := loadLegacy(ctx, func() error { return yinghuaAction.YingHuaLoginAction(cache) },
			func() ([]yinghuaAction.YingHuaCourse, error) { return yinghuaAction.CourseListAction(cache) },
			func(c yinghuaAction.YingHuaCourse) platform.Course {
				return platform.Course{Name: c.Name, Detail: fmt.Sprintf("已学视频 %d/%d，进度 %g", c.VideoLearned, c.VideoCount, c.Progress)}
			}, func() { yinghua.RunBrushOperation(setting, users, []*yinghuaAPI.YingHuaUserCache{cache}) })
		if err != nil {
			return nil, err
		}
		keepAliveCtx, stopKeepAlive := context.WithCancel(ctx)
		account.Close = stopKeepAlive
		// A manual selection may leave the authenticated session idle for several minutes.
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-keepAliveCtx.Done():
					return
				case <-ticker.C:
					status := yinghuaAPI.KeepAliveApi(*cache, 8)
					lg.Print(lg.INFO, "[", accountLabel(user), "] 登录心跳保活状态：", status)
				}
			}
		}()
		return account, nil
	case "ENAEA":
		cache := &enaeaAPI.EnaeaUserCache{Account: user.Account, Password: user.Password}
		return loadLegacy(ctx, func() error { _, err := enaeaAction.EnaeaLoginAction(cache); return err },
			func() ([]platform.Course, error) {
				projects, err := enaeaAction.ProjectListAction(cache)
				if err != nil {
					return nil, err
				}
				var courses []platform.Course
				for _, project := range projects {
					list, err := enaeaAction.CourseListAction(cache, project.CircleId)
					if err != nil {
						return nil, err
					}
					for _, c := range list {
						courses = append(courses, platform.Course{Name: project.ClusterName + " / " + c.CourseTitle, Detail: fmt.Sprintf("学习进度 %g", c.StudyProgress)})
					}
				}
				return courses, nil
			}, func(c platform.Course) platform.Course { return c },
			func() { enaea.RunBrushOperation(setting, users, []*enaeaAPI.EnaeaUserCache{cache}) })
	case "CQIE":
		cache := &cqieAPI.CqieUserCache{Account: user.Account, Password: user.Password}
		return loadLegacy(ctx, func() error { return cqieAction.CqieLoginAction(cache) },
			func() ([]cqieAction.CqieCourse, error) { return cqieAction.CqiePullCourseListAction(cache) },
			func(c cqieAction.CqieCourse) platform.Course {
				return platform.Course{Name: c.CourseName, Detail: fmt.Sprintf("已学单元 %d/%d，进度 %s", c.HaveUnit, c.SumUnit, c.Learned)}
			}, func() { cqie.RunBrushOperation(setting, users, []*cqieAPI.CqieUserCache{cache}) })
	case "KETANGX":
		cache := &ketangxAPI.KetangxUserCache{Account: user.Account, Password: user.Password}
		return loadLegacy(ctx, func() error { return ketangxAction.LoginAction(cache) },
			func() ([]ketangxAction.KetangxCourse, error) { return ketangxAction.PullCourseListAction(cache), nil },
			func(c ketangxAction.KetangxCourse) platform.Course {
				return platform.Course{Name: c.Title, Detail: fmt.Sprintf("学习进度 %g", c.Progress)}
			}, func() { ketangx.RunBrushOperation(setting, users, []*ketangxAPI.KetangxUserCache{cache}) })
	case "WELEARN":
		cache := &welearnAPI.WeLearnUserCache{Account: user.Account, Password: user.Password}
		return loadLegacy(ctx, func() error { return welearnAction.WeLearnLoginAction(cache) },
			func() ([]welearnAction.WeLearnCourse, error) { return welearnAction.WeLearnPullCourseListAction(cache) },
			func(c welearnAction.WeLearnCourse) platform.Course {
				return platform.Course{Name: c.Name, Detail: fmt.Sprintf("学习进度 %g", c.Per)}
			}, func() { welearn.RunBrushOperation(setting, users, []*welearnAPI.WeLearnUserCache{cache}) })
	case "ICVE":
		cache := &icveAPI.IcveUserCache{Account: user.Account, Password: user.Password}
		return loadLegacy(ctx, func() error {
			if len(user.Password) > 30 {
				return icveAction.IcveCookieLogin(cache)
			}
			return icveAction.IcveLoginAction(cache)
		}, func() ([]icveAction.IcveCourse, error) { return icveAction.PullZYKCourseAction(cache) },
			func(c icveAction.IcveCourse) platform.Course {
				return platform.Course{Name: c.CourseName, Detail: c.SchoolName + "，" + c.WeekStr}
			}, func() { icve.RunBrushOperation(setting, users, []*icveAPI.IcveUserCache{cache}) })
	case "QSXT":
		cache := &qsxtAPI.QsxtUserCache{Account: user.Account, Password: user.Password}
		return loadLegacy(ctx, func() error { _, err := qsxtAction.QsxtLoginAction(cache); return err },
			func() ([]qsxtAction.QsxtCourse, error) { return qsxtAction.PullCourseListAction(cache) },
			func(c qsxtAction.QsxtCourse) platform.Course {
				return platform.Course{Name: c.CourseName, Detail: c.StudyStatusName + "，" + c.SemesterName}
			}, func() { qsxt.RunBrushOperation(setting, users, []*qsxtAPI.QsxtUserCache{cache}) })
	case "HQKJ":
		cache := &hqkjAPI.HqkjUserCache{PreUrl: user.URL, Account: user.Account, Password: user.Password}
		return loadLegacy(ctx, func() error { return hqkjAction.HqkjLoginAction(cache) },
			func() ([]hqkjAction.HqkjCourse, error) { return hqkjAction.HqkjCourseListAction(cache) },
			func(c hqkjAction.HqkjCourse) platform.Course {
				return platform.Course{Name: c.Name, Detail: c.PeriodName}
			}, func() { haiqikeji.RunBrushOperation(setting, users, []*hqkjAPI.HqkjUserCache{cache}) })
	default:
		return nil, fmt.Errorf("平台 %q 尚无课程查询和刷课入口", user.AccountType)
	}
}

func loadLegacy[T any](ctx context.Context, login func() error, list func() ([]T, error), describe func(T) platform.Course, study func()) (*platform.Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := login(); err != nil {
		return nil, err
	}
	courses, err := list()
	if err != nil {
		return nil, err
	}
	account := &platform.Account{}
	for _, course := range courses {
		account.Courses = append(account.Courses, describe(course))
	}
	account.Study = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		study()
		return ctx.Err()
	}
	return account, nil
}
