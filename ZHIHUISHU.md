# 智慧树 AI 课程（控制台）

与 MOOC 共用 `feat/mooc-zhihuishu-study` 分支。当前接入智慧树的 **AI 课程**，不包含共享课、考试或能力测评。

## 登录与配置

1. 使用开启远程调试的专用 Edge/Chrome 浏览器，调试端口为 `9223`。
2. 在该浏览器访问 `https://onlineweb.zhihuishu.com/` 并登录，然后打开 AI 课程的章节页面。
3. 将下面的账户项加入现有配置的 `users` 列表，或在控制台配置向导中选择 `ZHIHUISHU`。
4. 运行控制台，确认浏览器当前账户正确后按回车。

```yaml
- accountType: ZHIHUISHU
  coursesCustom:
    videoModel: 0
    autoExam: 0
    examAutoSubmit: 0
    includeCourses: []
    excludeCourses: []
```

- `videoModel: 0`：验证登录并列出课程、模块和知识点。
- `videoModel: 1`：逐个处理支持的视频和电子书；视频每次等待实际经过的时间后上报，最大间隔 10 秒。
- `includeCourses` / `excludeCourses`：沿用现有课程名称筛选规则。
- 默认连接 `http://127.0.0.1:9223`，可使用 `YATORI_ZHIHUISHU_CDP_URL` 指定其他本机 IP 调试地址。
- 登录密码无需写入配置。Cookie 和动态协议材料仅在内存中传递，不输出或保存到配置。

## 行为与限制

- 视频支持完整视频及引用起止时间的片段；暂不支持的片段格式会提示手动处理。
- 已读取内容的电子书发送平台预览回执，其他资源类型提示手动处理。
- 登录失效时先访问课程门户恢复登录，再重新运行；页面要求额外验证时需人工完成。
- `Ctrl+C` 停止后不会补报尚未经过的时间。请求失败立即停止，不盲目重试写入。
- 进度日志表示接口接受了上报，不代表考试通过或课程已全部完成。
- 多课程切换、更多资源格式与课程最终完成状态尚需进一步实测。
- 当前入口用于控制台，尚未接入 Web 模式任务执行。